// factory.go — FrontendConfig.Options → Config translation with fail-loud
// validation. This package OWNS these option keys (unknown keys abort
// startup, same contract as unknown frontend/backend types):
//
//	passivePortMin / passivePortMax — passive data-channel port range
//	publicIP                        — PASV reply address
//	dataConnTimeoutSeconds          — seconds to wait for a passive data connect
package ftp

import (
	"crypto/tls"
	"fmt"
	"strconv"
)

// KnownOptionKeys lists every option key this frontend accepts.
var KnownOptionKeys = map[string]bool{
	"passivePortMin":         true,
	"passivePortMax":         true,
	"publicIP":               true,
	"dataConnTimeoutSeconds": true,
}

// ConfigFromOptions builds a Config from a frontends entry's options map.
// Unknown keys are a loud error naming the key; malformed values likewise.
// listenAddr and verifier are required and validated by New.
func ConfigFromOptions(listenAddr string, options map[string]string, tlsCfg *tls.Config, verifier PasswordVerifier) (Config, error) {
	cfg := Config{ListenAddr: listenAddr, TLSConfig: tlsCfg, Verifier: verifier}
	for k, v := range options {
		if !KnownOptionKeys[k] {
			return Config{}, fmt.Errorf("ftp: unknown option key %q (known: passivePortMin, passivePortMax, publicIP, dataConnTimeoutSeconds)", k)
		}
		switch k {
		case "passivePortMin":
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return Config{}, fmt.Errorf("ftp: passivePortMin %q must be a non-negative integer", v)
			}
			cfg.PassivePortMin = n
		case "passivePortMax":
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return Config{}, fmt.Errorf("ftp: passivePortMax %q must be a non-negative integer", v)
			}
			cfg.PassivePortMax = n
		case "dataConnTimeoutSeconds":
			// 0 means "use the library default" and is accepted; a negative
			// value is a config error, not a silent fallback.
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return Config{}, fmt.Errorf("ftp: dataConnTimeoutSeconds %q must be a non-negative integer", v)
			}
			cfg.DataConnTimeoutSeconds = n
		case "publicIP":
			cfg.PublicIP = v
		}
	}
	return cfg, nil
}
