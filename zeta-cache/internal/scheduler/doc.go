// Package scheduler is leaf 07: the three daemon loops (prompt upload,
// periodic sync, eviction) plus the quota/eviction policy engine.
//
// Package-name rationale (the leaf allowed internal/scheduler OR
// internal/quota): the loops ARE the majority of the surface (three of
// them vs one quota engine), the sync/prompt-upload loops have nothing to
// do with quota, and main.go wires ONE Start(ctx) for all three - so one
// scheduler package keeps the wiring line single. The quota/eviction
// engine lives in quota.go inside this package with no loop dependencies,
// so a future FileProvider share (master open question) can lift it out.
//
// # Loop x event state machine
//
//	prompt-upload loop (watcher):
//	  fs event on cache/files/**   -> dirty-set debounce (2s); path hits
//	                                  dirty index rows -> upload queue
//	  2s debounce fires            -> upload each queued path (serial);
//	                                  success -> done; failure -> stay
//	                                  dirty, retry on next event/sync
//	  sync running                 -> events COALESCE into the pending
//	                                  set (no queue growth); drained
//	                                  after the sync finishes
//	  ctx cancel                   -> stop watching; in-flight upload
//	                                  FINISHES (single-file atomicity)
//	  pause                        -> events coalesce only; uploads halt
//	                                  at the safe boundary (between
//	                                  files); resume drains
//
//	sync loop:
//	  start                        -> SyncOnce immediately (connect sync)
//	  success                      -> next sync at syncInterval (15m)
//	  failure                      -> status.state=error; next attempt at
//	                                  backoff (base 30s, x2 per
//	                                  consecutive failure, +jitter
//	                                  [0,25%) capped at 1h); a success
//	                                  resets backoff
//	  nightly tick (24h)           -> FullRescan (bypasses token skip)
//	  ctx cancel                   -> abort at the next engine boundary;
//	                                  shutdown < 2s asserted in tests
//	  pause                        -> timers hold; running SyncOnce is
//	                                  aborted via its ctx (safe
//	                                  boundary: the engine applies
//	                                  whole paths; a cancelled pass
//	                                  leaves the index consistent)
//
//	eviction loop:
//	  start                        -> pass immediately (reconcile may
//	                                  have left orphans)
//	  nightly tick                 -> eviction pass + tombstone prune
//	  usage crosses high-water     -> eviction pass until low-water or
//	                                  candidates exhausted (overflow
//	                                  reported via status)
//	  min-free device crossed      -> HARD STOP on writes (hydration and
//	                                  staging refuse); eviction still
//	                                  runs (it frees device space)
//	  macOS battery + discharging  -> pass DEFERRED until AC power
//	  ctx cancel                   -> stop; eviction is per-file atomic
//	  pause                        -> passes halt (quota pressure waits)
package scheduler
