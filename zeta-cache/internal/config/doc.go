// Package config loads and validates the zeta-cache configuration file.
//
// Fail-loud contract (mirrors the gateway's config validator): unknown
// JSON keys abort startup with the offending key named in the error, a
// missing required key aborts, and exactly one auth shape (Basic key
// pair OR mTLS client-certificate paths) must be configured.
package config
