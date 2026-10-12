// AppHost.swift - the container app: registers (or refreshes) the
// zeta-cache domain with the system, then parks. Run this to make the
// domain appear in Finder; quit it any time - the domain persists.
import AppKit
import FileProvider

let domain = NSFileProviderDomain(
    identifier: NSFileProviderDomainIdentifier("zeta-cache"),
    displayName: "zeta-cache")

let sem = DispatchSemaphore(value: 0)
NSFileProviderManager.getDomainsWithCompletionHandler { domains, err in
    if let err {
        print("GET ERR:", err)
        sem.signal()
        return
    }
    if domains.contains(where: { $0.identifier.rawValue == "zeta-cache" }) {
        print("DOMAIN ALREADY REGISTERED")
        sem.signal()
        return
    }
    NSFileProviderManager.add(domain) { addErr in
        if let addErr {
            print("ADD ERR:", addErr)
        } else {
            print("DOMAIN ADDED - look for 'zeta-cache' in Finder")
        }
        sem.signal()
    }
}
_ = sem.wait(timeout: .now() + 30)
