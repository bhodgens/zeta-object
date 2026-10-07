// Package fusefs owns the FUSE filesystem layer that serves the local cache
// dir: lookup/getattr/readdir/read/write/flush/fsync, hydration on read
// miss, write-through staging, and mount lifecycle. Owned by leaf 02
// (fuse-layer); leaf 01 ships only this placeholder and the daemon's
// unmount hook stub.
package fusefs
