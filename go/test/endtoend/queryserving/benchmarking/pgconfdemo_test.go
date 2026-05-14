// Copyright 2026 Supabase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package benchmarking

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/multigres/multigres/go/test/endtoend/shardsetup"
	"github.com/multigres/multigres/go/test/endtoend/suiteutil"
	"github.com/multigres/multigres/go/test/utils"
)

// demoPostgresMaxConnections sizes postgres to comfortably hold both
// multipooler's pool (globalCapacity 100 + admin 5) and pgbouncer's pool
// (default_pool_size 20), with headroom for superuser + replication
// reserved slots. The live demo will only open a handful, but we size for
// the configured ceiling so a stray script can't tip postgres over.
const demoPostgresMaxConnections = 200

// TestPGConfDemo stands up a dedicated multigres cluster (multigateway,
// multipoolers, pgctld, multiorch, multiadmin) alongside a pgbouncer
// instance in transaction mode pointed at the primary's postgres, prints
// the URLs and psql one-liners the speaker needs, and blocks until Ctrl-C.
//
// Intended for the PGConf.Dev 2026 "TEMP_TABLE survives COMMIT" live demo:
//  1. Show the multiadmin web UI (after running `pnpm dev` in web/multiadmin/
//     with MULTIADMIN_API_URL pointing at the harness's multiadmin HTTP port).
//  2. Open two psql sessions — one to multigateway, one to pgbouncer.
//  3. Paste the same SQL into both; multigres keeps the temp table across
//     COMMIT, pgbouncer transaction-mode does not.
//
// Enable with RUN_PGCONF_DEMO=1. Run with -timeout 0 so the blocking wait
// is not killed by go test's default 10-minute timeout. Example:
//
//	RUN_PGCONF_DEMO=1 go test -run TestPGConfDemo -timeout 0 \
//	    ./go/test/endtoend/queryserving/benchmarking/
//
// Uses a dedicated setup rather than the shared benchmarking cluster so
// the demo configuration (multiadmin enabled, restarted postgres) is
// isolated from other tests in this package.
func TestPGConfDemo(t *testing.T) {
	suiteutil.SkipUnlessEnabled(t, "RUN_PGCONF_DEMO")

	setup, cleanup := shardsetup.NewIsolated(t,
		shardsetup.WithMultipoolerCount(3), // HA shape: leader + 2 followers
		shardsetup.WithMultigateway(),
		shardsetup.WithMultiadmin(),
		shardsetup.WithLogLevel("warn"),
	)
	t.Cleanup(cleanup)
	setup.SetupTest(t)

	// Bump postgres max_connections so multipooler's pool (100 + admin 5)
	// and pgbouncer's pool (20) can both reach their configured size
	// without running into the default 60-slot ceiling. Restarts every
	// pgctld in the cluster.
	ctx := utils.WithTimeout(t, 10*time.Minute)
	bumpPostgresMaxConnections(ctx, t, setup, demoPostgresMaxConnections)
	time.Sleep(2 * time.Second) // let multipooler reconnect after pgctld restart

	primary := setup.GetPrimary(t)
	// WithServerResetQueryAlways forces DISCARD ALL on every backend
	// release, so the temp-table teardown is deterministically observable
	// from a single client. Without it pgbouncer in transaction mode
	// only loses session state when the pool actually churns to a
	// different backend — which doesn't happen reliably with one client.
	pgb, err := NewPgBouncerInstance(t, "localhost", primary.Pgctld.PgPort,
		shardsetup.DefaultTestUser, shardsetup.TestPostgresPassword,
		WithServerResetQueryAlways())
	if err != nil {
		t.Fatalf("failed to start pgbouncer: %v", err)
	}
	if pgb == nil {
		t.Fatal("pgbouncer binary not found on PATH — install pgbouncer to run this demo")
	}
	t.Cleanup(func() { pgb.Stop(t) })

	mgwDSN := shardsetup.GetTestUserDSN("localhost", setup.MultigatewayPgPort, "sslmode=disable")
	pgbDSN := shardsetup.GetTestUserDSN("localhost", pgb.Port(), "sslmode=disable")

	banner := fmt.Sprintf(`
================================================================================
  PGConf.Dev 2026 — "TEMP_TABLE survives COMMIT" demo
================================================================================

  PORTS (sanity-check before stage):
    multigateway (multigres):  %d   ← LEFT pane connects here
    pgbouncer (transaction):   %d   ← RIGHT pane connects here
    postgres (direct backend): %d   ← NOT used by either pane

  PGBOUNCER POOL MODE (set in the harness-generated pgbouncer.ini):
    pool_mode = transaction

  MULTIADMIN BACKEND:
    http://localhost:%d  (API)
    grpc://localhost:%d  (multigres CLI: --admin-server localhost:%d)

  MULTIADMIN WEB UI (run in a separate terminal, from repo root):
    MULTIADMIN_API_URL=http://localhost:%d pnpm --dir web/multiadmin dev
    Then open http://localhost:18100

  LEFT PANE — multigres / multipooler:
    PGPASSWORD=%s psql -h localhost -p %d -U %s postgres

  RIGHT PANE — pgbouncer (transaction mode):
    PGPASSWORD=%s psql -h localhost -p %d -U %s postgres

  Prove which pool each pane is on (run in EACH pane):
    \echo :SERVER_VERSION_NAME
      LEFT  → "17.0 (multigres)"   (multigateway's wire-protocol announce)
      RIGHT → real postgres version (e.g. "17.4")   (pgbouncer passes it through)

    \conninfo
      LEFT  → port "%d"
      RIGHT → port "%d"

  (Optional) Verify pgbouncer pool mode live (any third terminal):
    PGPASSWORD=%s psql -h localhost -p %d -U %s pgbouncer -c 'SHOW POOLS;'
    The "pool_mode" column reads "transaction" once the right pane has issued
    at least one query (pgbouncer only lists pools that have seen traffic).

  Paste this into both panes:
    BEGIN;
    CREATE TEMP TABLE demo (id int);
    INSERT INTO demo VALUES (1), (2), (3);
    COMMIT;
    SELECT * FROM demo;

  Left pane returns 3 rows. Right pane errors:
    ERROR:  relation "demo" does not exist

  DSNs (for tools that want them):
    multigateway: %s
    pgbouncer:    %s

  Press Ctrl-C in this terminal to tear everything down.
================================================================================
`,
		setup.MultigatewayPgPort, pgb.Port(), primary.Pgctld.PgPort,
		setup.MultiadminHttpPort,
		setup.MultiadminGrpcPort, setup.MultiadminGrpcPort,
		setup.MultiadminHttpPort,
		shardsetup.TestPostgresPassword, setup.MultigatewayPgPort, shardsetup.DefaultTestUser,
		shardsetup.TestPostgresPassword, pgb.Port(), shardsetup.DefaultTestUser,
		setup.MultigatewayPgPort, pgb.Port(),
		shardsetup.TestPostgresPassword, pgb.Port(), shardsetup.DefaultTestUser,
		mgwDSN, pgbDSN,
	)

	_, _ = os.Stderr.WriteString(banner)
	t.Logf("%s", banner)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	t.Logf("received %s — tearing down", sig)
}
