// main.swift - the .appex process entry. The system launches this
// binary via extensionKit; NSExtensionMain hands the XPC endpoint to the
// PlugInKit runtime, which instantiates the principal class named in the
// appex Info.plist (NSExtensionPrincipalClass) and drives the Replicated
// protocol through it. A bare dispatchMain() never registers the service
// endpoint - fileproviderd's bringup message finds no listener
// ("connection to service with pid -1").
import Foundation

// NSExtensionMain is C-only (Foundation's extension runtime entry);
// exposed to Swift here by its symbol.
@_silgen_name("NSExtensionMain")
func NSExtensionMain(_ argc: Int32, _ argv: UnsafeMutablePointer<UnsafeMutablePointer<CChar>?>) -> Int32

NSExtensionMain(CommandLine.argc, CommandLine.unsafeArgv)
