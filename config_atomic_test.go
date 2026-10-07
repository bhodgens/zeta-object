// config_atomic_test.go — test helper for the atomic serverConfig accessor.
//
// serverConfig is an atomic.Pointer-backed accessor (config.go); tests that
// mutate individual fields install the mutation through setServerConfigField,
// which snapshots the current generation, applies fn, and publishes the
// result as a new generation.
package main

// setServerConfigField applies fn to a copy of the current configuration and
// publishes the result atomically (test-only convenience over setServerConfig).
func setServerConfigField(fn func(c *ServerConfig)) {
	cfg := *serverConfig()
	fn(&cfg)
	setServerConfig(cfg)
}
