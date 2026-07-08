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
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	branchwal "github.com/argon-lab/argon/internal/branch/wal"
	"go.mongodb.org/mongo-driver/bson"
)

const (
	opMsg        = 2013
	opCompressed = 2012

	flagChecksumPresent = 1 << 0

	maxMessageSize = 48 * 1024 * 1024
	aliasSeparator = "~"
	resolveTTL     = 5 * time.Second
)

// ProjectLookup resolves project names to IDs.
type ProjectLookup interface {
	GetProjectIDByName(name string) (string, error)
}

// Proxy accepts client connections and forwards them to the upstream
// deployment with alias rewriting.
type Proxy struct {
	upstreamAddr string
	projects     ProjectLookup
	branches     *branchwal.BranchService

	cacheMu sync.Mutex
	cache   map[string]cachedTarget
}

type cachedTarget struct {
	physicalDB string
	err        error
	expires    time.Time
}

// New creates a proxy that dials upstreamAddr (host:port) for every client.
func New(upstreamAddr string, projects ProjectLookup, branches *branchwal.BranchService) *Proxy {
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

	upstream, err := net.Dial("tcp", p.upstreamAddr)
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

	// Client → server: frame messages and rewrite as needed.
	go func() {
		p.pump(client, upstream)
		done <- struct{}{}
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
		if length < 16 || length > maxMessageSize {
			log.Printf("wireproxy: dropping connection with invalid message length %d", length)
			return
		}
		message := make([]byte, length)
		copy(message, header)
		if _, err := io.ReadFull(client, message[4:]); err != nil {
			return
		}

		rewritten, errReply := p.rewrite(message)
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
// synthesized error reply to send back to the client instead.
func (p *Proxy) rewrite(message []byte) (forward []byte, errReply []byte) {
	opCode := int32(binary.LittleEndian.Uint32(message[12:16]))
	if opCode == opCompressed {
		// Should not happen — we strip compression from handshakes — but a
		// compressed alias command would slip through unrewritten, so pass
		// it along and let the server reject the unknown database loudly.
		log.Printf("wireproxy: unexpected OP_COMPRESSED message; compression negotiation slipped through")
		return message, nil
	}
	if opCode != opMsg {
		// OP_QUERY handshakes and other legacy ops pass through: their
		// database is admin/$cmd, never an alias.
		return message, nil
	}

	flagBits := binary.LittleEndian.Uint32(message[16:20])
	body := message[20:]
	if flagBits&flagChecksumPresent != 0 {
		body = body[:len(body)-4] // strip; recomputed on rewrite
	}

	// Section kind 0 (the command document) is first in practice; kind 1
	// document sequences follow it.
	if len(body) < 5 || body[0] != 0 {
		return message, nil
	}
	docLen := int(binary.LittleEndian.Uint32(body[1:5]))
	if docLen < 5 || 1+docLen > len(body) {
		return message, nil
	}
	rawDoc := bson.Raw(body[1 : 1+docLen])
	rest := body[1+docLen:]

	var doc bson.D
	if err := bson.Unmarshal(rawDoc, &doc); err != nil {
		return message, nil
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
				return nil, buildErrorReply(requestID, err.Error())
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
		return message, nil
	}

	newDoc, err := bson.Marshal(doc)
	if err != nil {
		return message, nil
	}

	// Reassemble: header + flags (checksum bit cleared) + section 0 + rest.
	out := make([]byte, 0, 20+1+len(newDoc)+len(rest))
	out = append(out, message[:16]...)
	var flags [4]byte
	binary.LittleEndian.PutUint32(flags[:], flagBits&^flagChecksumPresent)
	out = append(out, flags[:]...)
	out = append(out, 0)
	out = append(out, newDoc...)
	out = append(out, rest...)
	binary.LittleEndian.PutUint32(out[0:4], uint32(len(out)))
	return out, nil
}

// resolve maps "project~branch" to the physical database, with a short
// cache so hot paths don't hit metadata every command.
func (p *Proxy) resolve(alias string) (string, error) {
	p.cacheMu.Lock()
	if hit, ok := p.cache[alias]; ok && time.Now().Before(hit.expires) {
		p.cacheMu.Unlock()
		return hit.physicalDB, hit.err
	}
	p.cacheMu.Unlock()

	physical, err := p.lookup(alias)
	p.cacheMu.Lock()
	p.cache[alias] = cachedTarget{physicalDB: physical, err: err, expires: time.Now().Add(resolveTTL)}
	p.cacheMu.Unlock()
	return physical, err
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
