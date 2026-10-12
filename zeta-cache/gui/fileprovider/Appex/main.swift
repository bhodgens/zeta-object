// main.swift - the appex process entry. NSExtensionMain (linked from
// Foundation) boots the PlugInKit/extension runtime, registers the XPC
// endpoint fileproviderd connects to, instantiates the principal class
// named in Info.plist (NSExtensionPrincipalClass), and owns the run
// loop. Without this call the process starts with no listener and
// fileproviderd's initial bringup message finds nothing ("connection to
// service with pid -1").
import Foundation

@_silgen_name("NSExtensionMain")
func NSExtensionMain(_ argc: Int32, _ argv: UnsafeMutablePointer<UnsafeMutablePointer<CChar>?>) -> Int32

let _ = NSExtensionMain(CommandLine.argc, CommandLine.unsafeArgv)
