package wireproxy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

// stubLookups resolve one live branch, "shop~main", without a database.
type stubLookups struct{ lookups int }

func (s *stubLookups) GetProjectIDByName(name string) (string, error) {
	s.lookups++
	if name != "shop" {
		return "", errors.New("project not found")
	}
	return "project-1", nil
}

func (s *stubLookups) GetBranch(projectID, name string) (*wal.Branch, error) {
	switch {
	case projectID == "project-1" && name == "main":
		return &wal.Branch{ID: "b1", Name: "main", State: wal.BranchStateLive, PhysicalDB: "argon_br_b1"}, nil
	case projectID == "project-1" && name == "cold":
		return &wal.Branch{ID: "b2", Name: "cold"}, nil
	default:
		return nil, errors.New("branch not found")
	}
}

func newTestProxy() (*Proxy, *stubLookups) {
	stub := &stubLookups{}
	return New("127.0.0.1:1", stub, stub), stub
}

// opMsgFrame assembles an OP_MSG: header, flag bits, one kind-0 section and
// any trailing bytes (further sections, or a checksum).
func opMsgFrame(t testing.TB, flags uint32, command bson.D, trailer []byte) []byte {
	t.Helper()
	doc, err := bson.Marshal(command)
	require.NoError(t, err)
	frame := make([]byte, headerSize, headerSize+4+1+len(doc)+len(trailer))
	binary.LittleEndian.PutUint32(frame[4:8], 7) // requestID
	binary.LittleEndian.PutUint32(frame[12:16], opMsg)
	var flagBytes [4]byte
	binary.LittleEndian.PutUint32(flagBytes[:], flags)
	frame = append(frame, flagBytes[:]...)
	frame = append(frame, 0)
	frame = append(frame, doc...)
	frame = append(frame, trailer...)
	binary.LittleEndian.PutUint32(frame[0:4], uint32(len(frame)))
	return frame
}

// commandOf decodes the kind-0 command document of a forwarded OP_MSG.
func commandOf(t testing.TB, frame []byte) (bson.D, []byte) {
	t.Helper()
	require.GreaterOrEqual(t, len(frame), opMsgMinimum+5)
	require.EqualValues(t, len(frame), binary.LittleEndian.Uint32(frame[0:4]), "length header must match the frame")
	body := frame[opMsgMinimum:]
	require.EqualValues(t, 0, body[0])
	docLen := int(binary.LittleEndian.Uint32(body[1:5]))
	var doc bson.D
	require.NoError(t, bson.Unmarshal(body[1:1+docLen], &doc))
	return doc, body[1+docLen:]
}

func TestRewriteRoutesAliasToPhysicalDatabase(t *testing.T) {
	proxy, _ := newTestProxy()
	sequence := []byte{1, 2, 3, 4, 5} // an opaque kind-1 section must survive untouched
	frame := opMsgFrame(t, 0, bson.D{{Key: "find", Value: "orders"}, {Key: "$db", Value: "shop~main"}}, sequence)

	forward, reply, err := proxy.rewrite(frame)
	require.NoError(t, err)
	require.Nil(t, reply)
	command, rest := commandOf(t, forward)
	assert.Equal(t, bson.D{{Key: "find", Value: "orders"}, {Key: "$db", Value: "argon_br_b1"}}, command)
	assert.Equal(t, sequence, rest)
	assert.Equal(t, frame[4:16], forward[4:16], "request ID, responseTo and opcode are preserved")
}

func TestRewriteDropsChecksumOnlyWhenItRewrites(t *testing.T) {
	proxy, _ := newTestProxy()
	checksum := []byte{0xde, 0xad, 0xbe, 0xef}

	aliased := opMsgFrame(t, flagChecksumPresent, bson.D{{Key: "ping", Value: 1}, {Key: "$db", Value: "shop~main"}}, checksum)
	forward, _, err := proxy.rewrite(aliased)
	require.NoError(t, err)
	_, rest := commandOf(t, forward)
	assert.Empty(t, rest, "a rewritten frame no longer carries the stale checksum")
	assert.Zero(t, binary.LittleEndian.Uint32(forward[16:20])&flagChecksumPresent)

	plain := opMsgFrame(t, flagChecksumPresent, bson.D{{Key: "ping", Value: 1}, {Key: "$db", Value: "admin"}}, checksum)
	forward, _, err = proxy.rewrite(plain)
	require.NoError(t, err)
	assert.Equal(t, plain, forward, "non-alias traffic passes through byte for byte")
}

func TestRewriteNegotiatesCompressionAway(t *testing.T) {
	proxy, _ := newTestProxy()
	frame := opMsgFrame(t, 0, bson.D{
		{Key: "hello", Value: 1},
		{Key: "compression", Value: bson.A{"zstd", "snappy"}},
		{Key: "$db", Value: "admin"},
	}, nil)
	forward, _, err := proxy.rewrite(frame)
	require.NoError(t, err)
	command, _ := commandOf(t, forward)
	assert.Equal(t, bson.A{}, command[1].Value)
}

func TestRewriteAnswersUnresolvableAliases(t *testing.T) {
	proxy, _ := newTestProxy()
	for alias, want := range map[string]string{
		"nope~main": `project "nope" not found`,
		"shop~gone": `branch "gone" not found`,
		"shop~cold": "not checked out",
		"shop~":     "invalid branch alias",
		"~main":     "invalid branch alias",
	} {
		frame := opMsgFrame(t, 0, bson.D{{Key: "ping", Value: 1}, {Key: "$db", Value: alias}}, nil)
		forward, reply, err := proxy.rewrite(frame)
		require.NoError(t, err, alias)
		require.Nil(t, forward, alias)
		require.NotNil(t, reply, alias)
		assert.EqualValues(t, 7, binary.LittleEndian.Uint32(reply[8:12]), "the reply answers the request")
		command, _ := commandOf(t, reply)
		message := ""
		for _, field := range command {
			if field.Key == "errmsg" {
				message = fmt.Sprint(field.Value)
			}
		}
		assert.Contains(t, message, want, alias)
	}
}

// Every frame below once reached a slice expression with a length the
// client controls. None may panic; frames too short to be an OP_MSG are
// rejected, everything else is passed through for mongod to judge.
func TestRewriteNeverTrustsClientLengths(t *testing.T) {
	proxy, _ := newTestProxy()
	header := func(length int, op uint32) []byte {
		frame := make([]byte, length)
		binary.LittleEndian.PutUint32(frame[0:4], uint32(length))
		if length >= headerSize {
			binary.LittleEndian.PutUint32(frame[12:16], op)
		}
		return frame
	}
	withFlags := func(length int, flags uint32) []byte {
		frame := header(length, opMsg)
		binary.LittleEndian.PutUint32(frame[16:20], flags)
		return frame
	}
	hugeDocument := withFlags(opMsgMinimum+9, 0)
	binary.LittleEndian.PutUint32(hugeDocument[opMsgMinimum+1:], 0x7fffffff)
	negativeDocument := withFlags(opMsgMinimum+9, 0)
	binary.LittleEndian.PutUint32(negativeDocument[opMsgMinimum+1:], 0xffffffff)

	for name, tc := range map[string]struct {
		frame     []byte
		malformed bool
	}{
		"shorter than a header":            {make([]byte, 7), true},
		"OP_MSG header only (16 bytes)":    {header(16, opMsg), true},
		"OP_MSG with truncated flag bits":  {header(19, opMsg), true},
		"OP_MSG with flags but no section": {withFlags(20, 0), false},
		"checksum flag without a checksum": {withFlags(20, flagChecksumPresent), true},
		"checksum flag, 3 trailing bytes":  {withFlags(23, flagChecksumPresent), true},
		"checksum only, no section":        {withFlags(24, flagChecksumPresent), false},
		"document longer than the frame":   {hugeDocument, false},
		"document length overflows int32":  {negativeDocument, false},
		"legacy opcode, header only":       {header(16, 2004), false},
		"compressed opcode, header only":   {header(16, opCompressed), false},
	} {
		t.Run(name, func(t *testing.T) {
			var forward, reply []byte
			var err error
			require.NotPanics(t, func() { forward, reply, err = proxy.rewrite(tc.frame) })
			assert.Nil(t, reply)
			if tc.malformed {
				assert.ErrorIs(t, err, errMalformed)
				assert.Nil(t, forward)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.frame, forward)
			}
		})
	}
}

// The pump owns the connection: a malformed frame ends it without
// forwarding anything and without disturbing the process.
func TestPumpDropsMalformedFramesWithoutForwarding(t *testing.T) {
	proxy, _ := newTestProxy()
	clientSide, proxyClient := net.Pipe()
	proxyUpstream, upstreamSide := net.Pipe()
	defer func() { _ = clientSide.Close(); _ = upstreamSide.Close() }()

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		proxy.pump(proxyClient, proxyUpstream)
		_ = proxyUpstream.Close()
	}()

	bad := make([]byte, 16)
	binary.LittleEndian.PutUint32(bad[0:4], 16)
	binary.LittleEndian.PutUint32(bad[12:16], opMsg)
	go func() { _, _ = clientSide.Write(bad) }()

	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("pump did not stop on a malformed frame")
	}
	forwarded, err := io.ReadAll(upstreamSide)
	require.NoError(t, err)
	assert.Empty(t, forwarded)
}

func TestResolveCacheIsBoundedAndForgetsFailuresQuickly(t *testing.T) {
	proxy, stub := newTestProxy()

	_, err := proxy.resolve("shop~main")
	require.NoError(t, err)
	_, err = proxy.resolve("shop~main")
	require.NoError(t, err)
	assert.Equal(t, 1, stub.lookups, "a fresh hit is served from the cache")

	// A client inventing names must not grow the cache without bound.
	for i := 0; i < maxCachedAliases*3; i++ {
		_, _ = proxy.resolve(fmt.Sprintf("invented-%d~main", i))
	}
	proxy.cacheMu.Lock()
	size := len(proxy.cache)
	failure := proxy.cache[fmt.Sprintf("invented-%d~main", maxCachedAliases*3-1)]
	proxy.cacheMu.Unlock()
	assert.LessOrEqual(t, size, maxCachedAliases)
	assert.Error(t, failure.err)
	assert.LessOrEqual(t, time.Until(failure.expires), resolveErrorTTL)
}

// FuzzRewrite feeds arbitrary bytes through the frame parser. The property
// is the security boundary itself: no input may panic, and anything the
// proxy forwards or answers carries a truthful length header.
func FuzzRewrite(f *testing.F) {
	f.Add(opMsgFrame(f, 0, bson.D{{Key: "find", Value: "orders"}, {Key: "$db", Value: "shop~main"}}, nil))
	f.Add(opMsgFrame(f, flagChecksumPresent, bson.D{{Key: "ping", Value: 1}, {Key: "$db", Value: "shop~gone"}}, []byte{1, 2, 3, 4}))
	f.Add(opMsgFrame(f, 0, bson.D{{Key: "hello", Value: 1}, {Key: "compression", Value: bson.A{"zstd"}}, {Key: "$db", Value: "admin"}}, []byte{1, 0, 0, 0, 0}))
	short := make([]byte, 16)
	binary.LittleEndian.PutUint32(short[12:16], opMsg)
	f.Add(short)
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, frame []byte) {
		proxy, _ := newTestProxy()
		forward, reply, err := proxy.rewrite(frame)
		if err != nil {
			if forward != nil || reply != nil {
				t.Fatalf("a rejected frame must not produce output")
			}
			return
		}
		for _, out := range [][]byte{forward, reply} {
			if out == nil {
				continue
			}
			// Pass-through returns the caller's bytes unchanged; only frames
			// the proxy built itself promise a consistent header.
			if len(out) >= 4 && &out[0] != &frame[0] && int(binary.LittleEndian.Uint32(out[0:4])) != len(out) {
				t.Fatalf("length header %d does not match %d-byte output", binary.LittleEndian.Uint32(out[0:4]), len(out))
			}
		}
	})
}
