# wave2-B audit notes — zeta-cache scheduler / watcher / fusefs / ipc at HEAD 7f4a544

Read-only fix-efficacy + defect audit. Assigned findings: H4 H5 M20 M21 M24 M25 M26 M27 L48 L49 R2 R4 R6.
Assigned fix commits: 814d7c0 (scheduler), 83a137e (fusefs+ipc+main), b866aee (regression-review wave).

## Method / evidence

- Code read at HEAD (not the diffs alone), pre-fix code read with `git show <rev>^:<path>` and by
  extracting `git archive <rev>^ zeta-cache` into /tmp throwaway trees (no repo file touched; `git stash` never used).
- Real pre-fix test runs: the ADDED HEAD test files were copied into the pre-fix trees and run there.
  Results are quoted verbatim below — this is how each "would the test fail on the pre-fix code"
  question was answered instead of being guessed.

### Pre-fix test runs (verbatim outcomes)

| tree | added test | pre-fix result |
|------|-----------|----------------|
| 814d7c0^ | `TestSchedulerPauseResumeIntegration` (H4) | **PASS** (also PASS at `-count=10`) |
| 814d7c0^ | `TestWatcherPollFiresWithHealthyWatcher` (M20) | FAIL "poll tick never uploaded the dirty row with a healthy watcher" |
| 814d7c0^ | `TestWatcherCancelMidDrainDoesNotRearm` (M21) | FAIL "drain never started (no upload attempt)" |
| 83a137e^ | `TestH5WriteAfterFlushCommits` | FAIL `content = "Aemote generation", want "AB"` |
| 83a137e^ | `TestH5WriteAfterFsyncCommits` | FAIL `write after Fsync dropped: content = "Aemote generation"` |
| 83a137e^ | `TestM25DirSurvivesEntryTTL` | FAIL `dir inode replaced after entry TTL (M25): isDir=false` |
| 83a137e^ | `TestM26OpenWithOTruncDoesNotSilentlyTruncate` | FAIL `O_TRUNC silently truncated the cache: ""` |
| 83a137e^ | `TestStopReturnsWithSilentClient` / `TestConcurrentSilentClientDoesNotBlockOthers` | FAIL (5.10s i/o timeout — accept loop wedged) |
| 83a137e^ | `TestPinsWireNilEncodesAsEmptyArray` | FAIL `decoded pins is nil` |
| b866aee^ | `TestR4OTruncCloseFiresPromptUpload` | FAIL `UploadNow calls = [], want exactly [doc.txt]` |
| b866aee^ | `TestR2StopReturnsWithManySilentClients` + `TestR2StopRaceRepeated` | **PASS** (also PASS at `-count=30`) |
| b866aee^ | `r6_watcher_dedupe_test.go` | does not compile: `undefined: newSubtreeWatchers` |

`l49_resolve_test.go` cannot run pre-fix at all: the pre-fix `ConflictResolver.ResolveConflict`
returns only `error`, so the test file does not compile against the pre-fix interface
(compile-time enforcement, not a runtime pin).

### Gates at HEAD

- `cd zeta-cache && CGO_ENABLED=0 go test -count=1 ./internal/scheduler/... ./internal/fusefs/... ./internal/ipc/...` → ok/ok/ok.
- `go vet ./internal/scheduler/ ./internal/fusefs/ ./internal/ipc/` → clean (HEAD tree; the vet failure quoted above is the pre-fix tree refusing the post-fix helper).

## Assigned findings — verdicts

### H4 — scheduler pause channel close-and-replace. CLOSED (code) / test ineffective.
`scheduler.go:68-71` `wakeChs []chan struct{}`; `:106-110` one buffered-1 channel per loop in `New`;
`:295-302` `wake()` non-blocking token to EVERY loop; `:304-306` `loopWake(i)`; `:277-283` `setPaused`
broadcasts only on a real flip; the channels are never closed anywhere (grep: no `close(s.wakeChs…)`).
Consumers: `scheduler.go:179` (watcher), `:184` (sync loop), `:263` (eviction loop) — each holds its own.
Buffered-1 is what makes the resume token survive the window before the loop reaches its select
(`syncloop.go:41-49` hold loop; `watcher.go:277-282`). No shared-channel `select` remains.
TEST: `pause_test.go:170-236` ("post-resume interval pass", "scanned after 100ms = 1") PASSES on
814d7c0^ — 10/10 runs. The pre-fix bug was a busy-spin plus unreliable timers, and `select` still
reaches a ready timer by uniform tie-break, so pass-count assertions cannot see it. The test cannot fail.

### H5 — Flush `committed` latch never reset. CLOSED.
`fs.go:752-778`: the direct→staging reroute closes the superseded descriptor (`:773`) and resets the
latch (`:777`). `Flush`'s reopen closes the old fd before replacing it (`:829-832`). Both added H5 tests
fail on 83a137e^ with exactly the reported symptom, pass at HEAD.
Residual (LOW, not the reported defect): if the post-commit `os.Open(f.cachePath(key))` at `:828`
fails, `h.direct` stays false while `h.committed` is true and `h.stagingPath` keeps pointing at the
renamed-away staging path; the next `Flush` then returns 0 early (`:790`). The row is already dirty
(commitStaging), so bytes are not lost — only the prompt-upload hook/`MarkDirty` epoch is skipped
until the 30s poll. Fix: `h.stagingPath = f.cachePath(key)` unconditionally + clear `committed` when
the reopened handle is not direct.

### M20 — watcher poll fallback can never fire while fsnotify is healthy. CLOSED.
`watcher.go:232` `poll := w.clock.NewTimer(pollFallback)`, own select case `:305-324` re-arming at
`:310` regardless of watcher health; new subtrees re-watched at `:289-297` via `addSubtree`.
Test fails on 814d7c0^ as quoted. CLOSED.

### M21 — drain guard inverted / cancel branch unreachable. CLOSED.
`watcher.go:319-322` and `:331-334` are `if !w.drain(ctx, pending) { continue }` with the comment
matching `drain`'s contract (`:175-197` — returns false only on ctx cancel / a ctx.Canceled upload).
Test fails on 814d7c0^ (its reported failure mode there is M20's poll-never-fires, which is the same
code path — the pair is jointly pinned). CLOSED.

### M24 — IPC one connection at a time; Stop can hang. CLOSED.
`ipc.go:200-235` accept loop spawns one goroutine per conn; `:221` track; `:222` idle read deadline
armed before the goroutine; `:244-258` `trackConn` with the `stopped` flag; `:276-297` `Stop` closes
the listener, force-closes every tracked conn, then `wg.Wait()`. Both added tests FAIL pre-fix
(5.10s timeouts) and pass at HEAD. CLOSED.

### M25 — `localStat` never checked IsDir. CLOSED.
`store.go:60-69` returns `fi.IsDir()`; `fs.go:302-307` `statKey` propagates `lIsDir`; `fs.go:427`
`lookupChild` re-creates the child when the dir/file shape disagrees. Test fails pre-fix. CLOSED.

### M26 — client O_TRUNC reached the cache file + no dirty mark. CLOSED.
`fs.go:632-641` strips O_TRUNC/O_APPEND and records `dirtyNoCommit`; `fs.go:790-808` dirties the row
on close (and now fires the prompt hook — R4). Test fails pre-fix. Added verification: go-fuse v2.11.0
never advertises `CAP_ATOMIC_O_TRUNC` (only the print table references it), so the kernel sends the
separate SETATTR(size=0) → `Setattr` → hydrate+truncate+MarkDirty. The design assumption holds.
Probe `TestScratchTruncFlagReachesOpenFileCheck` confirms the stripped open leaves the bytes intact.

### M27 — `"pins":null` on the wire. CLOSED for pins; the same class is still open for two other payloads.
`ipc.go:70-84` `newPinData` normalises nil→`[]`, used at `:372` (pin ack) and `:383` (pins).
Test fails pre-fix. Accuracy note: the stated mechanism is imprecise — the Swift decoder uses
`decodeIfPresent` (`IPC.swift:80`), which TOLERATES JSON null; the real symptom was the whole extra
payload falling through to `throw DecodingError.dataCorrupted(... "unrecognized extra payload")`, so
`IPCClient.ask` throws and the menu silently shows no pins (no crash). Same live bug for
`ips.go:394`/`:452` (`TombstoneData{Tombstones: tombs}`) and `:433` (`ConflictData{Conflicts: items}`) —
see finding 4.

### L48 — dead seams. CLOSED.
`main.go:151` `Uploader: mountedFS` (scheduler prompt-upload path) and `main.go:174-183`
`mountedFS.SetUploader(uploaderFunc{…UploadFile…})` (FUSE flush hook) — the hook is real now and both
loops share one implementation. `ipc.DeletedLister` is gone (no hits anywhere in `*.go`).
Directories no longer get hydrated index rows: `fs.go:514-519` replaced the Mkdir `MarkClean` with an
explicit note, and the sync scan's dir rows are written with `Hydrated` unset (`sync/engine.go:495-498`,
`:525-527`) → hydrated=0, so the eviction predicate (clean hydrated rows, `quota.go:268-275`
`os.Remove`) can no longer select a directory. Caveat recorded: nothing sweeps orphaned staging —
see finding 3.

### L49 — `copyRemoved` echoed the request flag. CLOSED, with a residual inference.
`ipc.go:442-446` returns `ResolveData{… CopyRemoved: res.CopyRemoved}` from
`ResolveResult`; `gui_handler.go:135-159` computes it as `removeCopy && hadCopy` where `hadCopy` comes
from a pre-call `Conflicts()` snapshot, and `protocol.go:84-88` documents the fact contract. A record
with no copy + confirm=true now reports false (test `l49_resolve_test.go` pins exactly that).
Residual (LOW): the flag is still an inference, not the engine's own removal report —
`sync/conflicts.go:161-172` tolerates `os.IsNotExist` on the copy unlink, so a stale record whose copy
file is already gone still acks `copyRemoved=true`. The honest fix is a `ResolveResult` produced by
`Engine.ResolveConflict` itself (it already reads `rec.CopyPath` at `conflicts.go:162`), instead of
the handler re-deriving it from a second, earlier read.

### R2 — ipc Stop/accept race. CLOSED (code), test is not a pin.
`ipc.go:212-224` arms the deadline and tracks the conn inside the accept loop before `wg.Go`;
`:244-258` `trackConn` force-closes a conn accepted in the residual window (`stopped` under `connsMu`);
`:282-292` `Stop` sets `stopped` before the snapshot. Reasoned through the WaitGroup reuse hazard:
the accept loop is itself counted, so the counter cannot go 0→1 concurrently with `Wait` and the
"Add called concurrently with Wait" panic is unreachable. BOTH R2 tests PASS on b866aee^ (30/30):
the window is not exercised (closing a listening unix socket discards queued conns, so the dial-then-Stop
burst never gets a conn dequeued across the close). The "<2s pinned" claim in the commit message is
therefore unsupported by evidence.

### R4 — O_TRUNC-only close did not fire the prompt hook. CLOSED.
`fs.go:800-807` fires `f.up.UploadNow(ctx, h.key)` in the `dirtyNoCommit` branch, mirroring `:834-836`.
Test FAILS pre-fix (`UploadNow calls = []`) and passes at HEAD. CLOSED.

### R6 — watcher stacked a recursive watcher per Create+IsDir. PARTIAL.
The dedupe itself is real (`watcher.go:124-160` `subtreeWatchers`, `:248-256` claim/release-on-failure),
and it does bound the watcher count for a churn cycle. Two problems: (a) the claim set is never
released on directory delete, so the path is never re-watched — finding 1; (b) the regression test is
not a regression test — finding 7.

## New defects (findings returned to the parent)

1. MED ser/watcher.go:249-252 — R6 claim set never released on delete ⇒ recreated dir never re-watched.
2. MED fuse/fs.go:745+759 — read-back on a written handle fails EIO (O_WRONLY staging fd). Repro shown.
3. MED fuse/fs.go:857-859 — staging/ is never swept by anything, despite the doc claim; unbounded disk.
4. LOW-MED ipc/ipc.go:394,452,433 — tombstones/conflicts still marshal nil as `null` (M27 fixed pins only).
5. LOW-MED scheduler/pause_test.go:170 — H4 regression test passes on the pre-fix code (10/10).
6. LOW ipc/r2_stop_race_test.go — R2 tests pass on the pre-fix code (30/30).
7. LOW scheduler/r6_watcher_dedupe_test.go:28,52 — R6 test copies the production closure and needs a
   post-fix-only helper; does not compile pre-fix and its runnable assertion passes pre-fix.
8. LOW scheduler/watcher.go:104-120,226-229 — shutdown leaks the fan-in goroutines when mergeCh fills.

Proposed fixes are in the report; the two highest-value ones are the R6 `release` on Remove/Rename
(claim by inode, not path) and making the staging sweep real (Mount-time sweep of `staging/` older
than the process start, or wire the documented leaf-03 reconcile).
