# serverConfig global — writer/reader inventory (pre-fix, HEAD f54098c)

Prior commit f54098c added configMu (RWMutex) around some sites; race pin
test passes at HEAD, but parent mandates the atomic.Pointer pattern and
unguarded readers/writer-writer windows remain.

## WRITERS (production)
- config.go:418  `serverConfig = cfg` — loadConfig missing-file branch (startup + reload)
- config.go:490  `serverConfig = cfg` — loadConfig success branch (startup main.go:65 + reload config.go:604)
- main.go:118    applyListenAddrOverride(&serverConfig) — startup only
- Reload entry points: reloadIdentityRegistry (config.go:604) called from
  main_server.go:56 (SIGHUP goroutine) AND admin_wiring.go:203
  (adminReloadAuthService HTTP handler) — two concurrent writers possible.

## READERS — RUNTIME (concurrent with reload)
- s3_wiring.go:245      getBucketPath (locked via configMu at :243)
- config.go:384         configMuLockedZmetadBinary (locked) → admin_wiring.go:53,426
- backend_lazy.go:13    lazyDefaultBackendFor (locked)
- backend_lookup.go:168 lazyBackendFor (locked)
- actions.go:804-805    initializeInactivityTimers sweep (locked)
- main.go:186           bucketmanager.Custom closure — UNGUARDED
- config.go:560-567     buildIdentityRegistry (reload-path reader; only reload
  goroutines read it, but SIGHUP + POST /auth/reload can run concurrently)
- config.go:493-494     loadConfig log line reads AFTER unlock — UNGUARDED

## READERS — STARTUP ONLY (single-threaded, pre-listener)
- main.go:75,103-114,121-130,201,217
- main_server.go:34-35,83,138 (listener goroutines start; narrow windows)
- main_nonhttp.go:70
- frontends.go:144-145,185-186,325 (frontend factories during startupPlan)
- backend_lookup.go:182 initBackendLookup
- config_store.go:105   initConfigStore (takes address → must not alias)
- s3_wiring.go:317      installZfsDatasetProvisionerFor(&serverConfig)

## TEST WRITERS/READERS (~25 files, swap-and-restore pattern)
multipart_handlers_test.go, inactivity_runtime_test.go, config_backend_test.go,
frontends_coverage_test.go, backend_lookup_wiring_test.go, webdav_config_test.go,
actions_coverage_test.go, testshim_test.go, main_handler_test.go,
config_identities_test.go, multipart_sweeper_test.go, zfs_wiring_test.go,
root_coverage_wiring_test.go, main_lifecycle_test.go, main_quic_wiring_test.go,
client_ca_reload_test.go, ca_reloader_isolation_test.go,
config_zfsbucketdatasets_test.go, admin_wiring_test.go, config_store_test.go,
config_frontend_test.go, config_devmode_test.go, s3_wiring_test.go
