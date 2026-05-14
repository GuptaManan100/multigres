# PGConf.Dev 2026 — TEMP_TABLE survives COMMIT (live demo runbook)

This is the operator runbook for the slide-18 demo. The harness is
`TestPGConfDemo` in `pgconfdemo_test.go`. It spins up a real multigres
cluster in the HA shape we ship with — one multigateway, three
multipoolers (each with its own pgctld), a multiorch, and **multiadmin**
— plus a pgbouncer in transaction mode against the leader's postgres,
then parks the process and prints every URL and command you need.

## Prerequisites (do these once, before the day of the talk)

```bash
make build          # multigres binaries on PATH
brew install pgbouncer  # or apt-get install pgbouncer
pnpm --dir web/multiadmin install   # next.js deps for the admin UI
```

PostgreSQL 16+ must already be installed (`psql`, `postgres`, `initdb` on PATH).

---

## 1. Before you go on stage

You need three terminals, all working from the repo root.

**Terminal A — bring the cluster up and leave it running.**

```bash
RUN_PGCONF_DEMO=1 go test -v -run TestPGConfDemo -timeout 0 \
  ./go/test/endtoend/queryserving/benchmarking/
```

After ~60–90 seconds you'll see a banner. Copy it out — it contains every
port and command you'll use on stage. The important ones:

```text
MULTIADMIN BACKEND:
  http://localhost:<MA_HTTP>  (API)
  grpc://localhost:<MA_GRPC>  (multigres CLI: --admin-server localhost:<MA_GRPC>)

MULTIADMIN WEB UI (run in a separate terminal, from repo root):
  MULTIADMIN_API_URL=http://localhost:<MA_HTTP> pnpm --dir web/multiadmin dev

LEFT PANE  — multigres / multipooler:
  PGPASSWORD=<pw> psql -h localhost -p <MGW_PG>  -U postgres postgres

RIGHT PANE — pgbouncer (transaction mode):
  PGPASSWORD=<pw> psql -h localhost -p <PGB_PG>  -U postgres postgres
```

**Terminal B — start the multiadmin web UI**, pointed at the harness's
multiadmin HTTP port from the banner:

```bash
MULTIADMIN_API_URL=http://localhost:<MA_HTTP> pnpm --dir web/multiadmin dev
```

Open <http://localhost:18100> in your browser. Confirm you can see the
cells / databases / multipoolers list — that proves it's wired up.

**Terminals C and D — open the two psql sessions side-by-side**, each
running the `psql` line from the banner (left for multigres, right for
pgbouncer). Sanity-check with `SELECT 1;` in each.

**Final dry-run.** Paste the demo SQL block once into each pane and confirm
the expected outputs (3 rows on the left, `relation "demo" does not exist`
on the right). Then `\q` out of both and re-open them so on stage you
start from a clean slate.

---

## 2. On stage — show the deployment (multiadmin)

Switch to the browser tab with the multiadmin UI. Walk through:

- The **cells** page — one cell (`test-cell`).
- The **multipoolers** page — three poolers visible (one leader, two
  followers), all healthy. This is the HA shape we ship with.
- The **multigateways** page — one gateway routing to the shard.

Beat: _"This is what a multigres deployment looks like. One gateway,
three poolers in front of postgres for HA, all coordinated through
topology."_

---

## 3. On stage — show what each pane is connected to

The most compelling proof is server-driven, not client-config: multigres
announces itself in the wire protocol's `ParameterStatus`. In each pane
run:

```text
=> \echo :SERVER_VERSION_NAME
```

Outputs:

```text
LEFT (multigres):  17.0 (multigres)
RIGHT (pgbouncer): 17.4        ← the real postgres version, passed through
```

The string `(multigres)` only comes from multigateway's protocol layer
(`go/common/pgprotocol/server/startup.go`). pgbouncer is a transparent
PG-protocol proxy, so it forwards whatever the backing postgres reports.

For the port-level proof, `\conninfo` in each pane prints the port — the
banner's `PORTS` block tells you which port maps to which pool, including
the direct postgres port that **neither** pane should be on.

To show pgbouncer is in transaction mode, the banner already prints the
configured `pool_mode = transaction` line from the generated
`pgbouncer.ini`. If you want it live from pgbouncer itself, after the
right pane has issued at least one query, run from a third terminal:

```bash
PGPASSWORD=<pw> psql -h localhost -p <PGB_PG> -U postgres pgbouncer \
  -c 'SHOW POOLS;'
```

The `pool_mode` column on the `postgres / postgres` row reads
`transaction`. (`SHOW POOLS` only lists pools that have seen traffic;
`SHOW DATABASES` doesn't help here because `pool_mode` is set globally
in `pgbouncer.ini`, not per-database — that column is empty unless you
override per-DB.)

Beat: _"Same client (`psql`), same SQL, two different poolers — multigres
on the left announces itself in the wire protocol, pgbouncer on the right
is in standard transaction-pooling mode."_

---

## 4. On stage — run the queries

Paste the same block into **both** panes (have it on your clipboard):

```sql
BEGIN;
CREATE TEMP TABLE demo (id int);
INSERT INTO demo VALUES (1), (2), (3);
COMMIT;
SELECT * FROM demo;
```

Left pane:

```text
 id
----
  1
  2
  3
(3 rows)
```

Right pane:

```text
ERROR:  relation "demo" does not exist
LINE 1: SELECT * FROM demo;
                      ^
```

Punchline: _"Same SQL. Different pool. Different outcome. Multipooler
kept the session reserved because the bitmask saw a live temp table.
pgbouncer's transaction mode released the backend on COMMIT — and your
temp table went with it."_

### Why the harness sets `server_reset_query_always = 1`

pgbouncer's own default is `0`, which means in transaction mode the
backend is released to the pool **without** running `DISCARD ALL`. With
one client and no contention you almost always get the same backend back
on the next query — so the temp table appears to survive, and the demo
silently lies.

In production this fails randomly: as soon as another client steals the
backend between your `COMMIT` and your `SELECT`, the temp table is gone.
Setting `server_reset_query_always = 1` (see `WithServerResetQueryAlways`
in `pgbouncer.go`) forces `DISCARD ALL` on every release, so the failure
mode is deterministically observable from a single client. This matches
what production hits stochastically — we're not stacking the deck, we're
collapsing a probabilistic failure into a reliable one.

---

## Reset between practice runs

You don't need a reset script. The left pane's temp table dies with the
psql session; the right pane never created one. Either:

- `\q` out of both psql sessions and re-run the tmux launch command, **or**
- Just retype the SQL block — `CREATE TEMP TABLE demo` will fail on the
  left if you didn't `\q` (temp table still exists from the prior run),
  so for a clean redo, restart the sessions.

## Tear down

Ctrl-C in **Terminal A** (the go test harness). It propagates SIGINT,
runs all `t.Cleanup` hooks: pgbouncer stops, multiadmin stops, multipoolers
stop, pgctld gracefully stops postgres, etcd stops, temp dirs get removed.

Then Ctrl-C in **Terminal B** (pnpm dev) to stop the web UI.

## If something breaks mid-talk

The fallback per the talk plan is to advance to the next slide which
auto-plays a recorded run of this exact harness. The recording lives at
`demo/pgconf-2026/fallback.mp4` (TODO — capture from a successful local
run before the talk).

## Why this harness instead of docker-compose / hand-rolled scripts

Everything runs as one `go test` invocation against the e2e shardsetup
framework. That means:

- No separate postgres install for the pgbouncer side — pgbouncer points
  at the same postgres multipooler uses. The pitch sharpens: _same
  database, different pooler, different outcome._
- The same code paths exercised by CI's `TempTable_*` tests are what the
  audience sees on stage.
- Adding `WithMultiadmin()` to the shardsetup harness means future demos
  / tests can get the admin UI wired up with one option.
