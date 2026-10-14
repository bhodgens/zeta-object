// AppexSupport.swift - no main.swift here: Xcode-built app-extension
// targets get the extension runtime entry (NSExtensionMain) linked by
// the product type automatically; this file only documents that. Any
// additional ObjC-visible classes in the linked ZetaFileProviderReplicated
// product are discovered via NSExtensionPrincipalClass.
//
// L44: a top-level Appex/main.swift used to live next to this file and
// ALSO declare NSExtensionMain - two entry points in one target (the
// duplicate was deleted; this comment records why). The SwiftPM
// executable target that still needs an explicit main lives separately
// at Sources/ZetaFileProviderAppex/main.swift.
import Foundation
