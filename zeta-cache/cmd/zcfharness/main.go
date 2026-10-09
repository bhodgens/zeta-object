// zcfharness — the live CursorFeed verification harness (zfs-validate
// ztags-cursorfeed.py builds and runs this ON the validation host against
// the REAL gateway ?events endpoint). NOT part of the shipped zeta-cache
// binary; a thin driver around the module's own transport.Client +
// sync.NewCursorFeed so the live check exercises the production feed code
// path, not a re-implementation. The in-repo build (go test / vet) covers
// it; the deployed harness is cross-built from this same source.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/sync"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

func main() {
	url := os.Args[1]
	user, pass := os.Args[2], os.Args[3]
	bucket := os.Args[4]
	mode := os.Args[5] // "probe" or "delta"
	tmp := os.Args[6]

	cl, err := transport.NewClient(transport.Options{
		ServerURL: url, Bucket: bucket,
		BasicAuthUser: user, BasicAuthPass: pass,
		InsecureSkipVerify: true, // self-signed validation cert
	})
	if err != nil {
		fmt.Println("CLIENT_ERR:", err)
		os.Exit(2)
	}
	st, err := index.Open(filepath.Join(tmp, "idx-"+bucket+".db"))
	if err != nil {
		fmt.Println("INDEX_ERR:", err)
		os.Exit(2)
	}
	defer st.Close()
	feed, err := sync.NewCursorFeed(cl, st, bucket, nil)
	if err != nil {
		fmt.Println("FEED_ERR:", err)
		os.Exit(2)
	}
	ctx := context.Background()
	if mode == "probe" {
		fmt.Println("PROBE_FRESH:", feed.ProbeFresh(ctx))
		return
	}
	paths, fullScan, err := feed.Delta(ctx)
	if err != nil {
		fmt.Println("DELTA_ERR:", err)
		os.Exit(3)
	}
	fmt.Printf("DELTA: paths=%v fullScan=%v\n", paths, fullScan)
}
