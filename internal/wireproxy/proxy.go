// Package wireproxy is a MongoDB wire-protocol proxy that gives branches
// stable, human-readable connection strings:
//
//	mongodb://proxy-host:27018/<project>~<branch>?directConnection=true
//
// Clients speak ordinary MongoDB through it. The proxy rewrites the $db
// field of OP_MSG commands whose database is a branch alias
// ("project~branch") to the branch's physical database and forwards
// everything else untouched — responses stream back as a raw byte copy.
// Traffic to non-alias databases passes through completely unmodified, so
// the proxy is transparent for everything that isn't Argon's.
//
// Design constraints, stated honestly:
//
//   - Compression is negotiated away: the proxy strips the "compression"
//     field from handshake commands so the server never agrees to
//     OP_COMPRESSED (whose payloads we could not rewrite cheaply).
//   - Clients should use directConnection=true; topology discovery would
//     otherwise hand them the upstream's own address and they would
//     bypass the proxy.
//   - With authentication, use authSource=admin: the default authSource
//     is the URI database, which is the alias, and SCRAM must run against
//     a real database.
//   - Capture stays asynchronous (the change-stream ingester); the proxy
//     does not intercept writes and never will — the change stream hands
//     the ingester before/after images and mongod's commit order for free,
//     and re-deriving those here would mean reimplementing MongoDB write
//     semantics. A planned opt-in mode would hold a write's ack until the
//     ingester confirms the WAL entry (a synchronization barrier for
//     read-your-writes), which needs none of that parsing.
package wireproxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
)

const (
	opMsg        = 2013
	opCompressed = 2012

	flagChecksumPresent = 1 << 0

	// headerSize is the standard wire-protocol message header; an OP_MSG
	// body adds four flag bytes before its first section.
	headerSize    = 16
	opMsgMinimum  = headerSize + 4
	checksumBytes = 4

	maxMessageSize = 48 * 1024 * 1024
	aliasSeparator = "~"
	resolveTTL     = 5 * time.Second
	// upstreamDialTimeout keeps a client from waiting forever on an
	// unreachable deployment.
	upstreamDialTimeout = 10 * time.Second
	// Failed lookups are remembered only briefly: a client that connects a
	// moment before "argon checkout" finishes should not keep failing.
	resolveErrorTTL = time.Second
	// maxCachedAliases bounds the alias cache. Aliases are client-supplied
	// strings, so an unbounded map would grow with every distinct name a
	// client invents.
	maxCachedAliases = 1024
)

// errMalformed marks a frame that cannot be a well-formed wire message.
// The connection is dropped; nothing is forwarded upstream.
var errMalformed = errors.New("malformed wire message")

// ProjectLookup resolves project names to IDs.
type ProjectLookup interface {
	GetProjectIDByName(name string) (string, error)
}

// BranchLookup resolves a branch by project ID and name.
type BranchLookup interface {
	GetBranch(projectID, name string) (*wal.Branch, error)
}

// Proxy accepts client connections and forwards them to the upstream
// deployment with alias rewriting.
type Proxy struct {
	upstreamAddr string
	projects     ProjectLookup
	branches     BranchLookup

	cacheMu sync.Mutex
	cache   map[string]cachedTarget
}

type cachedTarget struct {
	physicalDB string
	err        error
	expires    time.Time
}

// New creates a proxy that dials upstreamAddr (host:port) for every client.
func New(upstreamAddr string, projects ProjectLookup, branches BranchLookup) *Proxy {
	return &Proxy{
		upstreamAddr: upstreamAddr,
		projects:     projects,
		branches:     branches,
		cache:        make(map[string]cachedTarget),
	}
}

// Serve accepts connections on the listener until the context is canceled.
func (p *Proxy) Serve(ctx context.Context, listener net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go p.handle(ctx, conn)
	}
}

func (p *Proxy) handle(ctx context.Context, client net.Conn) {
	defer func() { _ = client.Close() }()

	upstream, err := net.DialTimeout("tcp", p.upstreamAddr, upstreamDialTimeout)
	if err != nil {
		log.Printf("wireproxy: upstream dial failed: %v", err)
		return
	}
	defer func() { _ = upstream.Close() }()

	done := make(chan struct{}, 2)

	// Server → client: responses are never modified.
	go func() {
		_, _ = io.Copy(client, upstream)
		done <- struct{}{}
	}()

	// Client → server: frame messages and rewrite as needed. Frames are
	// validated before use, and a recover keeps any parsing defect confined
	// to this connection: the proxy is shared by every client, so one bad
	// frame must never take the process down.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("wireproxy: dropping connection after an unexpected frame error: %v", r)
			}
			done <- struct{}{}
		}()
		p.pump(client, upstream)
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}
}

// pump reads framed messages from the client, rewrites them, and writes
// them upstream. Synthesized error replies go straight back to the client.
func (p *Proxy) pump(client net.Conn, upstream net.Conn) {
	header := make([]byte, 4)
	for {
		if _, err := io.ReadFull(client, header); err != nil {
			return
		}
		length := int(binary.LittleEndian.Uint32(header))
		if length < headerSize || length > maxMessageSize {
			log.Printf("wireproxy: dropping connection with invalid message length %d", length)
			return
		}
		message := make([]byte, length)
		copy(message, header)
		if _, err := io.ReadFull(client, message[4:]); err != nil {
			return
		}

		rewritten, errReply, err := p.rewrite(message)
		if err != nil {
			log.Printf("wireproxy: dropping connection: %v", err)
			return
		}
		if errReply != nil {
			if _, err := client.Write(errReply); err != nil {
				return
			}
			continue
		}
		if _, err := upstream.Write(rewritten); err != nil {
			return
		}
	}
}

// rewrite returns the (possibly modified) message to forward, or a
// synthesized error reply to send back to the client instead. A frame that
// is too short to be what its header claims returns errMalformed and the
// caller drops the connection. Every length is checked before it is used:
// the bytes come straight from an unauthenticated socket.
func (p *Proxy) rewrite(message []byte) (forward []byte, errReply []byte, err error) {
	if len(message) < headerSize {
		return nil, nil, fmt.Errorf("%w: %d-byte message is shorter than a header", errMalformed, len(message))
	}
	opCode := int32(binary.LittleEndian.Uint32(message[12:16]))
	if opCode == opCompressed {
		// Should not happen — we strip compression from handshakes — but a
		// compressed alias command would slip through unrewritten, so pass
		// it along and let the server reject the unknown database loudly.
		log.Printf("wireproxy: unexpected OP_COMPRESSED message; compression negotiation slipped through")
		return message, nil, nil
	}
	if opCode != opMsg {
		// OP_QUERY handshakes and other legacy ops pass through: their
		// database is admin/$cmd, never an alias.
		return message, nil, nil
	}
	if len(message) < opMsgMinimum {
		return nil, nil, fmt.Errorf("%w: %d-byte OP_MSG has no flag bits", errMalformed, len(message))
	}

	flagBits := binary.LittleEndian.Uint32(message[16:20])
	body := message[opMsgMinimum:]
	if flagBits&flagChecksumPresent != 0 {
		if len(body) < checksumBytes {
			return nil, nil, fmt.Errorf("%w: OP_MSG declares a checksum it does not carry", errMalformed)
		}
		body = body[:len(body)-checksumBytes] // strip; recomputed on rewrite
	}

	// Section kind 0 (the command document) is first in practice; kind 1
	// document sequences follow it.
	if len(body) < 5 || body[0] != 0 {
		return message, nil, nil
	}
	docLen := int(binary.LittleEndian.Uint32(body[1:5]))
	if docLen < 5 || docLen > len(body)-1 {
		return message, nil, nil
	}
	rawDoc := bson.Raw(body[1 : 1+docLen])
	rest := body[1+docLen:]

	var doc bson.D
	if err := bson.Unmarshal(rawDoc, &doc); err != nil {
		return message, nil, nil
	}

	changed := false
	requestID := binary.LittleEndian.Uint32(message[4:8])

	for i, elem := range doc {
		switch elem.Key {
		case "$db":
			alias, ok := elem.Value.(string)
			if !ok || !strings.Contains(alias, aliasSeparator) {
				continue
			}
			physical, err := p.resolve(alias)
			if err != nil {
				return nil, buildErrorReply(requestID, err.Error()), nil
			}
			doc[i].Value = physical
			changed = true
		case "compression":
			// Never let the server agree to OP_COMPRESSED.
			doc[i].Value = bson.A{}
			changed = true
		}
	}
	if !changed {
		return message, nil, nil
	}

	newDoc, err := bson.Marshal(doc)
	if err != nil {
		return message, nil, nil
	}

	// Reassemble: header + flags (checksum bit cleared) + section 0 + rest.
	out := make([]byte, 0, opMsgMinimum+1+len(newDoc)+len(rest))
	out = append(out, message[:headerSize]...)
	var flags [4]byte
	binary.LittleEndian.PutUint32(flags[:], flagBits&^flagChecksumPresent)
	out = append(out, flags[:]...)
	out = append(out, 0)
	out = append(out, newDoc...)
	out = append(out, rest...)
	binary.LittleEndian.PutUint32(out[0:4], uint32(len(out)))
	return out, nil, nil
}

// resolve maps "project~branch" to the physical database, with a short
// cache so hot paths don't hit metadata every command.
func (p *Proxy) resolve(alias string) (string, error) {
	now := time.Now()
	p.cacheMu.Lock()
	if hit, ok := p.cache[alias]; ok && now.Before(hit.expires) {
		p.cacheMu.Unlock()
		return hit.physicalDB, hit.err
	}
	p.cacheMu.Unlock()

	physical, err := p.lookup(alias)
	ttl := resolveTTL
	if err != nil {
		ttl = resolveErrorTTL
	}
	p.cacheMu.Lock()
	if _, replacing := p.cache[alias]; !replacing && len(p.cache) >= maxCachedAliases {
		p.evictLocked(now)
	}
	p.cache[alias] = cachedTarget{physicalDB: physical, err: err, expires: now.Add(ttl)}
	p.cacheMu.Unlock()
	return physical, err
}

// evictLocked makes room in a full cache: expired entries go first, and if
// every entry is still fresh the cache is reset. Entries are cheap to
// rebuild (one metadata lookup), so a bounded map matters more than keeping
// each one.
func (p *Proxy) evictLocked(now time.Time) {
	for alias, target := range p.cache {
		if !now.Before(target.expires) {
			delete(p.cache, alias)
		}
	}
	if len(p.cache) >= maxCachedAliases {
		p.cache = make(map[string]cachedTarget)
	}
}

func (p *Proxy) lookup(alias string) (string, error) {
	parts := strings.SplitN(alias, aliasSeparator, 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("invalid branch alias %q (want project%sbranch)", alias, aliasSeparator)
	}
	projectID, err := p.projects.GetProjectIDByName(parts[0])
	if err != nil {
		return "", fmt.Errorf("project %q not found", parts[0])
	}
	branch, err := p.branches.GetBranch(projectID, parts[1])
	if err != nil {
		return "", fmt.Errorf("branch %q not found in project %q", parts[1], parts[0])
	}
	if !branch.IsLive() {
		return "", fmt.Errorf("branch %q is not checked out; run argon checkout first", parts[1])
	}
	return branch.PhysicalDB, nil
}

// buildErrorReply synthesizes an OP_MSG {ok: 0} response so drivers get a
// clean command error instead of a dropped connection.
func buildErrorReply(requestID uint32, message string) []byte {
	doc, err := bson.Marshal(bson.D{
		{Key: "ok", Value: 0},
		{Key: "errmsg", Value: "argon proxy: " + message},
		{Key: "code", Value: 26}, // NamespaceNotFound
		{Key: "codeName", Value: "NamespaceNotFound"},
	})
	if err != nil {
		return nil
	}
	out := make([]byte, 16, 16+4+1+len(doc))
	binary.LittleEndian.PutUint32(out[4:8], 0)          // requestID (server-side)
	binary.LittleEndian.PutUint32(out[8:12], requestID) // responseTo
	binary.LittleEndian.PutUint32(out[12:16], opMsg)
	var flags [4]byte
	out = append(out, flags[:]...)
	out = append(out, 0)
	out = append(out, doc...)
	binary.LittleEndian.PutUint32(out[0:4], uint32(len(out)))
	return out
}
