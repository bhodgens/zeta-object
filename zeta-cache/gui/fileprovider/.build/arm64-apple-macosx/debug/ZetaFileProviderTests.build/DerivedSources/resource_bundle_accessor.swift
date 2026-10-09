import Foundation

extension Foundation.Bundle {
    static let module: Bundle = {
        let mainPath = Bundle.main.bundleURL.appendingPathComponent("zeta-fileprovider_ZetaFileProviderTests.bundle").path
        let buildPath = "/Users/caimlas/git/mini-s3/zeta-cache/gui/fileprovider/.build/arm64-apple-macosx/debug/zeta-fileprovider_ZetaFileProviderTests.bundle"

        let preferredBundle = Bundle(path: mainPath)

        guard let bundle = preferredBundle ?? Bundle(path: buildPath) else {
            // Users can write a function called fatalError themselves, we should be resilient against that.
            Swift.fatalError("could not load resource bundle: from \(mainPath) or \(buildPath)")
        }

        return bundle
    }()
}