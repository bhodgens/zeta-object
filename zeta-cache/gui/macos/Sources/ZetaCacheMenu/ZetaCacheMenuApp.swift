// ZetaCacheMenuApp.swift - the macOS menu-bar app (leaf 08). A pure view
// over the daemon's IPC socket: status line, pause/resume, usage vs
// quota, conflicts submenu with resolve actions, pins submenu,
// recently-deleted with restore, and quit (GUI-only disconnect - the
// daemon keeps running as a login item).
//
// SwiftUI MenuBarExtra (macOS 13+), no Xcode project: `swift build`
// produces the .app-able binary; see README for the run recipe.

import SwiftUI

@main
struct ZetaCacheMenuApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate

    var body: some Scene {
        MenuBarExtra {
            MenuContentView(viewModel: delegate.viewModel)
        } label: {
            Image(systemName: delegate.viewModel.iconName)
        }
        .menuBarExtraStyle(.menu)
    }
}

final class AppDelegate: NSObject, NSApplicationDelegate {
    @MainActor let viewModel = MenuViewModel()

    func applicationDidFinishLaunching(_ notification: Notification) {
        viewModel.start()
    }
}

// MARK: - View model

@MainActor
final class MenuViewModel: ObservableObject {
    /// Default socket path: the daemon config's default ipcSocket
    /// (macOS runtime dir + zeta-cache.ipc). Overridable via the
    /// ZETA_CACHE_IPC env var for tests and non-default configs.
    var socketPath: String {
        if let env = ProcessInfo.processInfo.environment["ZETA_CACHE_IPC"], !env.isEmpty {
            return env
        }
        return NSTemporaryDirectory() + "zeta-cache.ipc"
    }

    @Published var status: StatusData?
    @Published var pins: [String] = []
    @Published var conflicts: [ConflictItem] = []
    @Published var tombstones: [Tombstone] = []
    @Published var connected = false

    private var refreshTimer: Timer?

    func start() {
        refresh()
        // Refresh on every menu open is handled by the view; this timer
        // keeps the icon honest in the background.
        refreshTimer = Timer.scheduledTimer(withTimeInterval: 15, repeats: true) { [weak self] _ in
            Task { @MainActor [weak self] in self?.refresh() }
        }
    }

    /// Icon reflects the sync state at a glance.
    var iconName: String {
        guard connected, let s = status else { return "questionmark.circle" }
        if s.paused { return "pause.circle" }
        switch s.state {
        case "syncing": return "arrow.triangle.2.circlepath.circle"
        case "error": return "exclamationmark.triangle"
        default: return "checkmark.circle"
        }
    }

    func refresh() {
        status = try? IPCClient.ask(socketPath, .status()).data
        connected = (status != nil)
        if let resp = try? IPCClient.ask(socketPath, .pins()),
           case .pins(let p) = resp.extra {
            pins = p
        } else {
            pins = []
        }
        if let resp = try? IPCClient.ask(socketPath, .conflictsList()),
           case .conflicts(let c) = resp.extra {
            conflicts = c
        } else {
            conflicts = []
        }
        if let resp = try? IPCClient.ask(socketPath, .deletedList()),
           case .tombstones(let t) = resp.extra {
            tombstones = t
        } else {
            tombstones = []
        }
    }

    // MARK: Commands (each re-refreshes so the menu shows the result)

    func togglePause() {
        let req = (status?.paused ?? false) ? IPCRequest.resume() : IPCRequest.pause()
        _ = try? IPCClient.ask(socketPath, req)
        refresh()
    }

    func setPin(_ path: String, _ pinned: Bool) {
        _ = try? IPCClient.ask(socketPath, .pin(path, pinned))
        refresh()
    }

    func resolve(_ conflict: ConflictItem, _ mode: ResolveMode, removeCopy: Bool) {
        _ = try? IPCClient.ask(socketPath, .conflictsResolve(conflict.path, mode, confirm: removeCopy))
        refresh()
    }

    func restore(_ tombstone: Tombstone) {
        _ = try? IPCClient.ask(socketPath, .deletedRestore(tombstone.path))
        refresh()
    }

    // MARK: Derived display values

    var stateLine: String {
        guard let s = status else { return "daemon not reachable" }
        if s.paused { return "paused" }
        switch s.state {
        case "syncing": return "syncing…"
        case "error": return "error: \(s.lastError ?? "unknown")"
        case "idle": return "synced"
        default: return s.state
        }
    }

    var lastSyncLine: String {
        guard let s = status, let secs = s.lastSync?.unixSeconds, secs > 0 else {
            return "never synced"
        }
        let date = Date(timeIntervalSince1970: TimeInterval(secs))
        let fmt = DateFormatter()
        fmt.dateStyle = .none
        fmt.timeStyle = .short
        return "last sync \(fmt.string(from: date))"
    }

    var usageLine: String {
        guard let s = status else { return "" }
        let mb = Double(s.usageBytes) / 1_048_576
        if s.capBytes > 0 {
            let capMb = Double(s.capBytes) / 1_048_576
            return String(format: "%.0f MB of %.0f MB cache", mb, capMb)
        }
        return String(format: "%.0f MB cached (no cap)", mb)
    }

    /// 0...1 fraction for the usage bar (nil = uncapped).
    var usageFraction: Double? {
        guard let s = status, s.capBytes > 0 else { return nil }
        return min(Double(s.usageBytes) / Double(s.capBytes), 1.0)
    }
}

// MARK: - Menu content

struct MenuContentView: View {
    @ObservedObject var viewModel: MenuViewModel

    var body: some View {
        // Status line block
        Text(viewModel.stateLine)
        Text(viewModel.lastSyncLine).font(.caption)
        Divider()

        // Usage vs quota
        Text(viewModel.usageLine)
        if let frac = viewModel.usageFraction {
            ProgressView(value: frac)
                .frame(width: 140)
                .padding(.horizontal)
        }
        Divider()

        // Pause / resume toggle
        Button(viewModel.status?.paused == true ? "Resume Syncing" : "Pause Syncing") {
            viewModel.togglePause()
        }
        Divider()

        // Conflicts submenu
        Menu("Conflicts (\(viewModel.conflicts.count))") {
            if viewModel.conflicts.isEmpty {
                Text("No conflicts").font(.caption)
            } else {
                ForEach(viewModel.conflicts, id: \.path) { conflict in
                    Menu(conflict.path) {
                        Button("Keep My Version (upload)") {
                            viewModel.resolve(conflict, .keepLocal, removeCopy: false)
                        }
                        Button("Keep Server Version (download)") {
                            viewModel.resolve(conflict, .keepRemote, removeCopy: false)
                        }
                        Button("Keep Server Version and Delete My Copy…") {
                            viewModel.resolve(conflict, .keepRemote, removeCopy: true)
                        }
                    }
                }
            }
        }

        // Pins submenu
        Menu("Pins (\(viewModel.pins.count))") {
            if viewModel.pins.isEmpty {
                Text("No pinned files").font(.caption)
            } else {
                ForEach(viewModel.pins, id: \.self) { pin in
                    Button("Unpin \(pin)") {
                        viewModel.setPin(pin, false)
                    }
                }
            }
        }

        // Recently deleted submenu
        Menu("Recently Deleted (\(viewModel.tombstones.count))") {
            if viewModel.tombstones.isEmpty {
                Text("Nothing recently deleted").font(.caption)
            } else {
                ForEach(viewModel.tombstones, id: \.path) { tomb in
                    Button("Restore \(tomb.path)") {
                        viewModel.restore(tomb)
                    }
                }
            }
        }
        Divider()

        // Quit disconnects the GUI ONLY; the daemon keeps running.
        Button("Quit Menu App (daemon keeps running)") {
            NSApplication.shared.terminate(nil)
        }
    }
}
