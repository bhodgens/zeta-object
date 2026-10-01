// sshserver.go — the SSH transport: ServerConfig with the password /
// public-key auth callbacks, per-connection session channel loop, and the
// "sftp" subsystem routing to sftp.RequestServer over the driver Handlers.
package sftp

import (
	"log"
	"net"
	"sort"

	"golang.org/x/crypto/ssh"

	"github.com/pkg/sftp"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// handleConn runs one SSH connection: handshake (auth callbacks resolve the
// identity), session-channel loop, sftp subsystem → RequestServer.Serve.
func (f *Frontend) handleConn(conn net.Conn) {
	defer conn.Close()

	// The identity resolved by the auth callback rides the Permissions'
	// critical options (a string the callbacks control); Permissions is the
	// only per-connection state ssh.ServerConfig hands back.
	cfg := &ssh.ServerConfig{
		PasswordCallback: f.passwordCallback(),
	}
	// Public-key auth is offered ONLY when a KeyChecker is wired (nil
	// checker = pubkey auth not offered, matching the documented config
	// contract). Installing the callback unconditionally would nil-panic
	// inside f.cfg.KeyChecker.Accept on the first publickey offer.
	if f.cfg.KeyChecker != nil {
		cfg.PublicKeyCallback = func(md ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			id, ok := f.cfg.KeyChecker.Accept(key)
			if !ok {
				log.Printf("sftp: public-key auth failed for %q from %s", md.User(), md.RemoteAddr())
				return nil, errAuthFailed
			}
			return identityPermissions(id), nil
		}
	}
	signer, generated, fingerprint, err := loadOrGenerateHostKey(f.cfg.HostKeyFile)
	if err != nil {
		log.Printf("sftp: %v", err)
		return
	}
	if generated {
		log.Printf("sftp: generated new host key at %s (fingerprint %s) — clients can pin it from now on", f.cfg.HostKeyFile, fingerprint)
	}
	cfg.AddHostKey(signer)

	sconn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		// Auth failures land here client-side; log at debug-level brevity.
		return
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			if err := newChannel.Reject(ssh.UnknownChannelType, "only session channels are supported"); err != nil {
				log.Printf("sftp: channel reject: %v", err)
			}
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go f.handleSession(sconn.Permissions, channel, requests)
	}
}

// passwordCallback returns the password callback (nil when password auth is
// disabled — ssh then never offers "password" as a method).
func (f *Frontend) passwordCallback() func(md ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
	if !f.cfg.AllowPasswordAuth {
		return nil
	}
	return func(md ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		id, ok := f.cfg.Verifier.Verify(md.User(), string(password))
		if !ok {
			log.Printf("sftp: password auth failed for %q from %s", md.User(), md.RemoteAddr())
			return nil, errAuthFailed
		}
		return identityPermissions(id), nil
	}
}

// identityPermissions encodes the resolved identity into ssh.Permissions
// (CriticalOptions map: accessKeyID → grants rendering). This is the only
// channel the auth callbacks have to carry state to the session handler.
func identityPermissions(id auth.Identity) *ssh.Permissions {
	return &ssh.Permissions{
		CriticalOptions: map[string]string{
			"accessKeyID": id.AccessKeyID,
			"grants":      renderGrants(id),
		},
	}
}

// renderGrants renders the grant map deterministically ("bucket=value,..."
// sorted by bucket) so the session handler can reconstruct the identity and
// the round-trip is stable. Read=false must stay false: the value encodes
// exactly what the identity holds ("-" = nothing, "r", "rw").
func renderGrants(id auth.Identity) string {
	buckets := make([]string, 0, len(id.BucketGrants))
	for bucket := range id.BucketGrants {
		buckets = append(buckets, bucket)
	}
	sort.Strings(buckets)
	out := ""
	for _, bucket := range buckets {
		g := id.BucketGrants[bucket]
		value := "-"
		switch {
		case g.Read && g.Write:
			value = "rw"
		case g.Read:
			value = "r"
		}
		if out != "" {
			out += ","
		}
		out += bucket + "=" + value
	}
	return out
}

// identityFromPermissions reconstructs the auth.Identity the auth callback
// resolved (round-trip through CriticalOptions is lossless for grants —
// the FLOOR map, leaf 09: the rich table is deliberately NOT serialized
// through CriticalOptions).
func identityFromPermissions(p *ssh.Permissions) auth.Identity {
	id := auth.Identity{BucketGrants: map[string]auth.Grant{}}
	if p == nil {
		return id
	}
	id.AccessKeyID = p.CriticalOptions["accessKeyID"]
	for _, pair := range splitComma(p.CriticalOptions["grants"]) {
		bucket, value, ok := cut(pair, "=")
		if !ok || bucket == "" {
			continue
		}
		switch value {
		case "rw":
			id.BucketGrants[bucket] = auth.Grant{Read: true, Write: true}
		case "r":
			id.BucketGrants[bucket] = auth.Grant{Read: true}
		default:
			id.BucketGrants[bucket] = auth.Grant{} // "-" = explicit nothing
		}
	}
	return id
}

// RichGrantsResolver re-resolves an identity's rich grant table by access
// key ID (leaf 09). The registry adapter implements it; tests inject fakes.
type RichGrantsResolver interface {
	RichGrantsFor(accessKeyID string) []auth.GrantExpr
}

// resolveRichGrants returns the rich entries stored under accessKeyID
// (nil for legacy-only identities ⇒ identical wire behavior).
func resolveRichGrants(id auth.Identity, resolver RichGrantsResolver) []auth.GrantExpr {
	if resolver == nil {
		return nil
	}
	return resolver.RichGrantsFor(id.AccessKeyID)
}

// handleSession serves one session channel: wait for the sftp subsystem
// request, then hand the channel to a RequestServer bound to the
// connection's resolved identity.
func (f *Frontend) handleSession(perms *ssh.Permissions, channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()
	for req := range requests {
		subsystem, ok := classifySubsystemRequest(req)
		if !ok {
			if req.WantReply {
				if err := req.Reply(false, nil); err != nil {
					log.Printf("sftp: request reply: %v", err)
				}
			}
			continue
		}
		if subsystem != "sftp" {
			if req.WantReply {
				if err := req.Reply(false, nil); err != nil {
					log.Printf("sftp: request reply: %v", err)
				}
			}
			continue
		}
		if req.WantReply {
			if err := req.Reply(true, nil); err != nil {
				log.Printf("sftp: request reply: %v", err)
			}
		}
		id := identityFromPermissions(perms)
		// Leaf 09: re-resolve the rich grant table from the registry at
		// session start (one lookup, cached for the session — same
		// bounded-staleness property as today's handshake-time auth). The
		// CriticalOptions carried only the floor; the rich entries were
		// never serialized through them.
		id = id.WithRichGrants(resolveRichGrants(id, f.cfg.RichGrants))
		server := sftp.NewRequestServer(channel, f.handlers(id))
		if err := server.Serve(); err != nil {
			log.Printf("sftp: subsystem serve ended: %v", err)
		}
		return
	}
}

// classifySubsystemRequest validates a session request as a subsystem
// open. ssh.Request payloads are length-prefixed: byte 0..3 is the
// big-endian name length, [4:] the name. Anything that is not a
// "subsystem" request, or whose payload is too short to carry the length
// prefix + a non-empty name (len < 5), is rejected — a short or hostile
// payload must NEVER reach the [4:] slice (a 4-byte slice of a shorter
// payload panics the session goroutine and, unrecovered, the process).
// ok=false → the request is not a subsystem request at all (Reply(false)
// and skip); ok=true with a name → it is one (serve only when name=="sftp").
func classifySubsystemRequest(req *ssh.Request) (name string, ok bool) {
	if req == nil || req.Type != "subsystem" {
		return "", false
	}
	if len(req.Payload) < 5 {
		return "", false
	}
	return string(req.Payload[4:]), true
}

// splitComma / cut are small stdlib-shaped helpers.
func splitComma(s string) []string {
	var out []string
	start := 0
	for i := range len(s) {
		if s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) || len(out) > 0 {
		out = append(out, s[start:])
	}
	return out
}

func cut(s, sep string) (before, after string, found bool) {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}
