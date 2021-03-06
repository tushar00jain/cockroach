// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package sql_test

import (
	gosql "database/sql"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"

	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
)

func TestTrace(t *testing.T) {
	defer leaktest.AfterTest(t)()

	s := log.Scope(t)
	defer s.Close(t)

	// These are always appended, even without the test specifying it.
	alwaysOptionalSpans := []string{
		"[async] storage.pendingLeaseRequest: requesting lease",
		"range lookup",
	}

	testData := []struct {
		name          string
		getRows       func(t *testing.T, sqlDB *gosql.DB) (*gosql.Rows, error)
		expSpans      []string
		optionalSpans []string
	}{
		{
			name: "Session",
			getRows: func(t *testing.T, sqlDB *gosql.DB) (*gosql.Rows, error) {
				if _, err := sqlDB.Exec("SET DISTSQL = OFF"); err != nil {
					t.Fatal(err)
				}
				// Start session tracing.
				if _, err := sqlDB.Exec("SET TRACING = ON"); err != nil {
					t.Fatal(err)
				}

				// Run some query
				rows, err := sqlDB.Query(`SELECT * FROM test.foo`)
				if err != nil {
					t.Fatal(err)
				}
				if err := rows.Close(); err != nil {
					t.Fatal(err)
				}

				// Stop tracing and extract the trace
				if _, err := sqlDB.Exec("SET TRACING = OFF"); err != nil {
					t.Fatal(err)
				}

				return sqlDB.Query(
					"SELECT DISTINCT(operation) op FROM crdb_internal.session_trace " +
						"WHERE operation IS NOT NULL ORDER BY op")
			},
			expSpans: []string{
				"sql txn implicit",
				"/cockroach.roachpb.Internal/Batch",
			},
		},
		{
			name: "SessionDistSQL",
			getRows: func(t *testing.T, sqlDB *gosql.DB) (*gosql.Rows, error) {
				if _, err := sqlDB.Exec("SET DISTSQL = ON"); err != nil {
					t.Fatal(err)
				}

				// Start session tracing.
				if _, err := sqlDB.Exec("SET TRACING = ON"); err != nil {
					t.Fatal(err)
				}

				// Run some query
				rows, err := sqlDB.Query(`SELECT * FROM test.foo`)
				if err != nil {
					t.Fatal(err)
				}
				if err := rows.Close(); err != nil {
					t.Fatal(err)
				}

				// Stop tracing and extract the trace
				if _, err := sqlDB.Exec("SET TRACING = OFF"); err != nil {
					t.Fatal(err)
				}

				return sqlDB.Query(
					"SELECT DISTINCT(operation) op FROM crdb_internal.session_trace " +
						"WHERE operation IS NOT NULL ORDER BY op")
			},
			expSpans: []string{
				"sql txn implicit",
				"flow",
				"table reader",
				"/cockroach.roachpb.Internal/Batch",
			},
			// Depending on whether the data is local or not, we may not see these
			// spans.
			optionalSpans: []string{
				"/cockroach.sql.distsqlrun.DistSQL/SetupFlow",
				"noop",
			},
		},
		{
			name: "ShowTraceFor",
			getRows: func(_ *testing.T, sqlDB *gosql.DB) (*gosql.Rows, error) {
				if _, err := sqlDB.Exec("SET DISTSQL = OFF"); err != nil {
					t.Fatal(err)
				}
				return sqlDB.Query(
					"SELECT DISTINCT(operation) op FROM [SHOW TRACE FOR SELECT * FROM test.foo] " +
						"WHERE operation IS NOT NULL ORDER BY op")
			},
			expSpans: []string{
				"sql txn implicit",
				"starting plan",
				"consuming rows",
				"/cockroach.roachpb.Internal/Batch",
			},
		},
		{
			name: "ShowTraceForSplitBatch",
			getRows: func(_ *testing.T, sqlDB *gosql.DB) (*gosql.Rows, error) {
				if _, err := sqlDB.Exec("SET DISTSQL = OFF"); err != nil {
					t.Fatal(err)
				}

				// Deleting from a multi-range table will result in a 2PC transaction
				// and will split the underlying BatchRequest/BatchResponse. Tracing
				// in the presence of multi-part batches is what we want to test here.
				return sqlDB.Query(
					"SELECT DISTINCT(operation) op FROM [SHOW TRACE FOR DELETE FROM test.bar] " +
						"WHERE message LIKE '%1 DelRng%' ORDER BY op")
			},
			expSpans: []string{
				"kv.DistSender: sending partial batch",
				"starting plan",
				"/cockroach.roachpb.Internal/Batch",
			},
		},
	}

	// Create a cluster. We'll run sub-tests using each node of this cluster.
	const numNodes = 3
	cluster := serverutils.StartTestCluster(t, numNodes, base.TestClusterArgs{})
	defer cluster.Stopper().Stop(context.TODO())

	clusterDB := cluster.ServerConn(0)
	if _, err := clusterDB.Exec(`
		CREATE DATABASE test;

		--- test.foo is a single range table.
		CREATE TABLE test.foo (id INT PRIMARY KEY);

		--- test.bar is a multi-range table.
		CREATE TABLE test.bar (id INT PRIMARY KEY);
		ALTER TABLE  test.bar SPLIT AT VALUES (5);
	`); err != nil {
		t.Fatal(err)
	}

	for _, test := range testData {
		test.optionalSpans = append(test.optionalSpans, alwaysOptionalSpans...)
		sort.Strings(test.expSpans)

		t.Run(test.name, func(t *testing.T) {
			// Session tracing needs to work regardless of whether tracing is enabled, so
			// we're going to test both cases.
			//
			// We'll also check traces from all nodes. The point is to be sure that we
			// test a node that is different than the leaseholder for the range, so that
			// the trace contains remote spans.
			for _, enableTr := range []bool{false, true} {
				name := "TracingOff"
				if enableTr {
					name = "TracingOn"
				}
				t.Run(name, func(t *testing.T) {
					for i := 0; i < numNodes; i++ {
						t.Run(fmt.Sprintf("node-%d", i), func(t *testing.T) {
							// Use a new "session" for each sub-test rather than
							// cluster.ServerConn() so that per-session state has a known
							// value. This is necessary for the check below that the
							// session_trace starts empty.
							//
							// TODO(andrei): Pull the check for an empty session_trace out of
							// the sub-tests so we can use cluster.ServerConn(i) here.
							pgURL, cleanup := sqlutils.PGUrl(
								t, cluster.Server(i).ServingAddr(), "TestTrace", url.User(security.RootUser))
							defer cleanup()
							sqlDB, err := gosql.Open("postgres", pgURL.String())
							if err != nil {
								t.Fatal(err)
							}
							defer sqlDB.Close()

							sqlDB.SetMaxOpenConns(1)

							// Run a non-traced read to acquire a lease on the table, so that the
							// traced read below doesn't need to take a lease. Tracing a lease
							// acquisition incurs some spans that are too fragile to test here.
							if _, err := sqlDB.Exec(`SELECT * FROM test.foo LIMIT 1`); err != nil {
								t.Fatal(err)
							}

							if _, err := cluster.ServerConn(0).Exec(
								fmt.Sprintf(`SET CLUSTER SETTING trace.debug.enable = %t`, enableTr),
							); err != nil {
								t.Fatal(err)
							}

							// Sanity check that new sessions don't have trace info on them.
							row := sqlDB.QueryRow("SELECT COUNT(1) FROM crdb_internal.session_trace")
							var count int
							if err := row.Scan(&count); err != nil {
								t.Fatal(err)
							}
							if count != 0 {
								t.Fatalf("expected crdb_internal.session_trace to be empty "+
									"at the beginning of a session, but it wasn't. Count: %d.", count)
							}

							rows, err := test.getRows(t, sqlDB)
							if err != nil {
								t.Fatal(err)
							}
							defer rows.Close()

							ignoreSpans := make(map[string]bool)
							for _, s := range test.optionalSpans {
								ignoreSpans[s] = true
							}
							r := 0
							for rows.Next() {
								var op string
								if err := rows.Scan(&op); err != nil {
									t.Fatal(err)
								}
								if ignoreSpans[op] {
									continue
								}
								if r >= len(test.expSpans) {
									t.Fatalf("extra span: %s", op)
								}
								if op != test.expSpans[r] {
									t.Fatalf("expected span: %q, got: %q", test.expSpans[r], op)
								}
								r++
							}
							if r < len(test.expSpans) {
								t.Fatalf("missing expected spans: %s", test.expSpans[r:])
							}
						})
					}
				})
			}
		})
	}
}

func TestBracketInTracetags(t *testing.T) {
	defer leaktest.AfterTest(t)()

	query := "SELECT 42"

	params := base.TestServerArgs{
		Knobs: base.TestingKnobs{
			SQLExecutor: &sql.ExecutorTestingKnobs{
				BeforeExecute: func(ctx context.Context, stmt string, isParallel bool) {
					if strings.Contains(stmt, query) {
						taggedCtx := log.WithLogTag(ctx, "hello", "[::666]")
						log.Event(taggedCtx, "world")
					}
				},
			},
		},
	}
	s, sqlDB, _ := serverutils.StartServer(t, params)
	defer s.Stopper().Stop(context.TODO())

	sqlDB.SetMaxOpenConns(1)

	if _, err := sqlDB.Exec("SET tracing = ON"); err != nil {
		t.Fatal(err)
	}

	if _, err := sqlDB.Exec(query); err != nil {
		t.Fatal(err)
	}

	if _, err := sqlDB.Exec("SET tracing = OFF"); err != nil {
		t.Fatal(err)
	}

	rows, err := sqlDB.Query(`SELECT message, context FROM [SHOW TRACE FOR SESSION];`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	ok := false
	for rows.Next() {
		var msg, ct []byte
		if err := rows.Scan(&msg, &ct); err != nil {
			t.Fatal(err)
		}
		t.Logf("received trace: %q // %q", msg, ct)
		if len(ct) > 0 && ct[0] == '[' {
			if ct[len(ct)-1] != ']' {
				t.Errorf("context starts with open bracket but does not close it: %q", ct)
			}
		}
		c1 := strings.Count(string(ct), "[")
		c2 := strings.Count(string(ct), "]")
		if c1 != c2 {
			t.Errorf("mismatched brackets: %q", ct)
		}
		if string(msg) == "world" && strings.Contains(string(ct), "hello=[::666]") {
			ok = true
		}
	}
	if !ok {
		t.Fatal("expected message not found in trace")
	}
}
