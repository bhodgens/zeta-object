package metadata

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

var (
	regMu sync.RWMutex
	reg   = map[string]MetadataProvider{}
)

// Register adds a provider under its Name(). Panics on duplicate names
// (programmer error; registration happens at startup only).
func Register(p MetadataProvider) {
	regMu.Lock()
	defer regMu.Unlock()
	if existing, ok := reg[p.Name()]; ok {
		panic("metadata: provider already registered: " + p.Name() +
			" (existing type: " + nameOf(existing) + ")")
	}
	reg[p.Name()] = p
}

// Lookup returns the provider registered under name, or nil.
func Lookup(name string) MetadataProvider {
	regMu.RLock()
	defer regMu.RUnlock()
	return reg[name]
}

// ProbeAndAttach probes every registered provider for bucketPath and returns
// the names of providers whose Probe reports Available. Probe errors are
// treated as "not available" (the provider's Reason or the error text is
// preserved for logging by callers via ProbeResult fields where possible)
// and never abort the loop. bucketPath must be symlink-resolved by the
// caller.
//
// The returned names are sorted so startup logs and CapabilitySet contents
// are deterministic.
func ProbeAndAttach(ctx context.Context, bucketPath string) []string {
	regMu.RLock()
	providers := make([]MetadataProvider, 0, len(reg))
	for _, p := range reg {
		providers = append(providers, p)
	}
	regMu.RUnlock()

	var attached []string
	for _, p := range providers {
		res, err := p.Probe(ctx, bucketPath)
		if err != nil {
			continue // probe failure = not available; logged by caller hook
		}
		if res.Available {
			attached = append(attached, p.Name())
		}
	}
	sort.Strings(attached)
	return attached
}

// nameOf is a narrow fmt-free type-name helper for panic messages.
func nameOf(p MetadataProvider) string {
	return fmt.Sprintf("%T", p)
}
