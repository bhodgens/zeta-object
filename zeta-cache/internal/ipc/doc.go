// Package ipc serves the zeta-cache local control protocol over a Unix
// domain socket: versioned JSON, one request per connection, every message
// carries v and type. Leaf 01 implements the status request; leaf 08
// (gui-ipc) owns the fuller protocol.
package ipc
