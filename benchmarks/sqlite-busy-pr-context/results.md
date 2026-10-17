# PR-context stale-snapshot contention (#57)

Issue: https://github.com/vifar/no-mistakes/issues/57

The installed binary's clean build revision is
`d0b67c525f0fab1c436a15d2d8648bbad83e8c3e` (`go version -m`).
Its actual writer is `internal/db/pr_context.go`, not the generated T3 Code
`binCli.mjs` inspected in the initial brief. The state DB already uses WAL and
`busy_timeout(5000)`. Neither setting changes in this fix.

## Outside proof

The fixture in `internal/db/pr_context_contention_test.go` starts a child test
process using the production `BindRunPRContext` and the real modernc SQLite
driver. A test-only driver wrapper places a pipe barrier after the receipt
SELECT's rows close, before the INSERT. The parent commits through an
independent connection to the same disposable database before releasing the
child. No live state DB or live root configuration is used.

Both runs use fixture SHA256
`e47e4c6f57cd40a0fcedb71f04020d581c37af45b9033a7c84462d2274d398b7`.

| Evidence | Revision | Exit | Observation | Log SHA256 |
| --- | --- | --- | --- | --- |
| [red.log](red.log) | `8bbb83a5c157f365f7927b94322cc36d95f02817` | 1 | One attempt returns the reported write-receipt error, extended code 517 (`SQLITE_BUSY_SNAPSHOT`). | `0101b5dd268b904756bae6636751d794c2641800e23d30a091a59ca57c0ccbb5` |
| [green.log](green.log) | `573027f8db5b26c787423386b8889a5987dd2775` | 0 | Recovery uses two transactions, reads the competitor's generation 2, then commits generation 3 and invalidates review authority. Four forced stale snapshots exhaust the budget and preserve the previous receipt and authority. | `bfe347fe0fd910af534e7f516822788482f875f3a26222617d80888a35bb439e` |

The command and exact head appear in each log. Run again with:

```sh
go test ./internal/db -run '^TestPRContextContention$' -v -count=1
```

## Recovery contract

The retry surrounds the entire bind/advance transaction. Each attempt rolls
back before waiting and re-reads identity, generation, and authority. There are
at most four attempts, with 25/50/100 ms backoff. The existing 5000 ms SQLite busy
handler may also wait on ordinary lock contention; the total backoff is 175 ms,
not a new wall-clock timeout. `errors.As` extracts `*sqlite.Error`, and
`Code() & 0xff == SQLITE_BUSY` includes extended BUSY codes such as 517.
`SQLITE_LOCKED` and arbitrary error text do not select this retry.

The unrelated T3 bundle's 5000 ms paths set up persistence/read workers; its
100 ms paths read Antigravity/OpenCode usage databases; its zero-timeout
exclusive transaction deliberately holds a separate `server-owner.sqlite`
ownership lock. None identifies or configures this Go writer.

## Operational record

The hub applied cap 1 on both roots around 2026-10-17 10:31Z. Controller capacity
was 3; general capacity was 2. No restart, pause, cancellation, or displacement:
controller had one occupied slot and zero queued; general had zero occupied
slots and zero queued. Parked run status labels are not occupied-slot counts.
The submission-level runner gate has not landed and remains future work.

No install or merge is authorized here. The hub owns installation approval and
restoration of the two prior capacity values after installation on both roots.
A recurrence after installation means stop and report to the hub; it is evidence
the fix is wrong, not permission to retry/relaunch a failed production run or
increase its retry budget.
