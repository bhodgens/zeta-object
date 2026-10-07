# Leaf 08 - GUI status app + IPC protocol

## Goal

The macOS-first menu-bar status app and the local IPC protocol it speaks
to the daemon. The daemon owns all state; the GUI is a view plus
command surface (pause/resume, conflict resolution, pins, recently-
deleted restore). Linux tray app is DEFERRED (after the daemon
stabilizes) - this leaf delivers the macOS app + the versioned IPC
protocol both platforms will use.

## Requirements

- IPC protocol (extend leaf 01's `status` request into a full v1):
  JSON over a Unix domain socket, one request per connection (simpler
  than framing), every message has `v` and `type`. Requests:
  `status`, `pause`, `resume`, `conflicts.list`, `conflicts.resolve`
  (keep-local | keep-remote, path), `pins.list`, `pins.add` (path),
  `pins.remove` (path), `deleted.list`, `deleted.restore` (path),
  `evict` (path, admin-ish: evict one clean file now). Responses:
  `{v:1, ok:true, data:...}` or `{v:1, ok:false, error:"..."}`.
  Unknown type = ok:false "unknown request type" (forward-compatible).
- Daemon side: implement the handlers on the leaf-04/07 state; conflicts
  come from the sync engine's conflict list (leaf 04 surfaces it);
  restore = re-download via transport if the server still serves the
  path (or a version of it), else honest error; pause/resume = suspend
  the scheduler loops (in-flight upload finishes or aborts at a safe
  boundary - document which).
- macOS menu-bar app: SwiftUI menu bar extra (StatusBar) - decide
  Swift-vs-Go HERE per the master's open question; the locked shape is
  menu items: state line (synced/syncing/error + last sync),
  pause/resume toggle, usage vs quota bar, conflicts submenu (count ->
  resolve actions), pins submenu, recently-deleted submenu (restore),
  quit (quit disconnects GUI only - the daemon keeps running as a login
  item; document launchd/LaunchAgent wiring for the daemon).
- The GUI never talks to the network; ONLY the daemon does (security
  property: one network-attached process; the GUI talks to the local
  socket, permission-checked by file mode 0600 socket in the user's
  runtime dir).
- The Swift/Go split (if Swift wins): the extension is a thin client of
  the IPC socket; JSON types mirrored in Swift with Codable; a
  protocol-conformance test runs the JSON fixtures through both the Go
  encoder and a golden file.
- Linux tray app: DEFERRED. Leave the IPC protocol platform-neutral
  (no macOS-only fields) and note the deferral in the README.

## Constraints

- No new server interaction (restore uses existing transport Get).
- The GUI holds no credentials, no certs, no config write access
  (pause/resume/pins go through the daemon; pins persist in daemon
  config via the daemon's own config-write path).
- App distribution: unsigned local build is fine for v1; notarized
  packaging is future work (note in README).

## Acceptance

1. IPC protocol tests: every request type handled, unknown-type
   rejection, concurrent clients, socket permission 0600, daemon
   restart drops clients cleanly (they reconnect).
2. Pause/resume semantics tested (scheduler loops actually stop; a
   paused daemon still serves FUSE reads from cache).
3. Conflict resolve tested: keep-local (upload wins), keep-remote
   (download wins, local conflicted copy removed after user confirm
   flag), both via the table.
4. GUI: build instructions in the report; if Swift, a minimal Xcode
   project or SPM package compiles and shows the menu with a live
   status fetched over IPC (manual verification steps documented - the
   parent cannot drive GUI).
5. README section: GUI setup, LaunchAgent snippet, IPC protocol
   reference table.
