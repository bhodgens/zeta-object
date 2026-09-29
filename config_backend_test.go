package main

import (
	"encoding/json"
	"testing"
)

// TestLoadConfigBackendKeysBackwardCompat pins the leaf-03 config schema:
// both the legacy bare-string bucket value and the new object form parse,
// and the backends map loads. JSON keys are frozen here.
func TestLoadConfigBackendKeysBackwardCompat(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		verify  func(t *testing.T, c ServerConfig)
		wantErr bool
	}{
		{
			name: "legacy string bucket value still parses",
			json: `{"dataDir":"./data/","buckets":{"photos":"/mnt/photos"}}`,
			verify: func(t *testing.T, c ServerConfig) {
				if c.Buckets["photos"] != "/mnt/photos" {
					t.Fatalf("Buckets[photos] = %q, want /mnt/photos", c.Buckets["photos"])
				}
			},
		},
		{
			name: "object bucket value with backend",
			json: `{"buckets":{"photos":{"path":"/mnt/photos","backend":"fs"}}}`,
			verify: func(t *testing.T, c ServerConfig) {
				if c.Buckets["photos"] != "/mnt/photos" {
					t.Fatalf("Buckets[photos] = %q, want /mnt/photos", c.Buckets["photos"])
				}
				if c.BucketBackends["photos"] != "fs" {
					t.Fatalf("BucketBackends[photos] = %q, want fs", c.BucketBackends["photos"])
				}
			},
		},
		{
			name: "object bucket value without backend defaults empty",
			json: `{"buckets":{"photos":{"path":"/mnt/photos"}}}`,
			verify: func(t *testing.T, c ServerConfig) {
				if c.Buckets["photos"] != "/mnt/photos" {
					t.Fatalf("Buckets[photos] = %q, want /mnt/photos", c.Buckets["photos"])
				}
				if c.BucketBackends["photos"] != "" {
					t.Fatalf("BucketBackends[photos] = %q, want empty", c.BucketBackends["photos"])
				}
			},
		},
		{
			name: "backends map loads",
			json: `{"backends":{"fs":{"root":"./data/"}}}`,
			verify: func(t *testing.T, c ServerConfig) {
				if c.Backends["fs"].Root != "./data/" {
					t.Fatalf("Backends[fs].Root = %q, want ./data/", c.Backends["fs"].Root)
				}
			},
		},
		{
			name: "backends options load",
			json: `{"backends":{"proxy":{"root":"","options":{"endpoint":"http://s3.local"}}}}`,
			verify: func(t *testing.T, c ServerConfig) {
				if c.Backends["proxy"].Options["endpoint"] != "http://s3.local" {
					t.Fatalf("options lost: %+v", c.Backends["proxy"])
				}
			},
		},
		{
			name: "no new keys at all parses as before",
			json: `{"dataDir":"./d/","listenAddr":":9999"}`,
			verify: func(t *testing.T, c ServerConfig) {
				if c.DataDir != "./d/" || c.ListenAddr != ":9999" {
					t.Fatalf("basic keys broken: %+v", c)
				}
				if c.Buckets == nil {
					t.Fatal("Buckets should decode to empty, not break")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c ServerConfig
			err := json.Unmarshal([]byte(tc.json), &c)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if err == nil {
				tc.verify(t, c)
			}
		})
	}
}
