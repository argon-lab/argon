package wireproxy

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func queryHandshake(t testing.TB, namespace string, command bson.D, trailer []byte) ([]byte, int) {
	t.Helper()
	doc, err := bson.Marshal(command)
	require.NoError(t, err)
	frame := make([]byte, headerSize+4)
	binary.LittleEndian.PutUint32(frame[4:8], 19)
	binary.LittleEndian.PutUint32(frame[12:16], 2004)
	binary.LittleEndian.PutUint32(frame[16:20], 4) // secondaryOk
	frame = append(frame, namespace...)
	frame = append(frame, 0)
	frame = append(frame, make([]byte, 8)...)
	binary.LittleEndian.PutUint32(frame[len(frame)-4:], 0xffffffff)
	docStart := len(frame)
	frame = append(frame, doc...)
	frame = append(frame, trailer...)
	binary.LittleEndian.PutUint32(frame[0:4], uint32(len(frame)))
	return frame, docStart
}

func TestLegacyHandshakeDisablesCompression(t *testing.T) {
	for _, command := range []string{"hello", "isMaster", "ismaster"} {
		for _, compression := range []bson.A{nil, {}, {"zlib", "zstd", "snappy"}} {
			t.Run(command+"/"+compressionName(compression), func(t *testing.T) {
				proxy, _ := newTestProxy()
				doc := bson.D{{Key: command, Value: int32(1)}, {Key: "helloOk", Value: true}}
				if compression != nil {
					doc = append(doc, bson.E{Key: "compression", Value: compression})
				}
				projection, err := bson.Marshal(bson.D{{Key: "ok", Value: 1}})
				require.NoError(t, err)
				frame, docStart := queryHandshake(t, "admin.$cmd", doc, projection)
				forward, reply, err := proxy.rewrite(frame)
				require.NoError(t, err)
				require.Nil(t, reply)
				assert.Equal(t, frame[4:docStart], forward[4:docStart], "header and query fields survive")
				docLength := int(binary.LittleEndian.Uint32(forward[docStart:]))
				var got bson.D
				require.NoError(t, bson.Unmarshal(forward[docStart:docStart+docLength], &got))
				if compression == nil {
					assert.Equal(t, frame, forward)
				} else {
					doc[2].Value = bson.A{}
					assert.Equal(t, doc, got)
				}
				assert.Equal(t, projection, forward[docStart+docLength:])
				assert.EqualValues(t, len(forward), binary.LittleEndian.Uint32(forward[:4]))
			})
		}
	}
}

func compressionName(value bson.A) string {
	if value == nil {
		return "absent"
	}
	if len(value) == 0 {
		return "empty"
	}
	return "requested"
}

func TestLegacyNonHandshakePassesThrough(t *testing.T) {
	for _, namespace := range []string{"admin.orders", "admin.$cmd"} {
		proxy, _ := newTestProxy()
		frame, _ := queryHandshake(t, namespace, bson.D{{Key: "find", Value: "orders"}, {Key: "compression", Value: bson.A{"zlib"}}}, nil)
		forward, reply, err := proxy.rewrite(frame)
		require.NoError(t, err)
		assert.Equal(t, frame, forward)
		assert.Nil(t, reply)
	}
}

func TestCompressedMessageIsNeverForwarded(t *testing.T) {
	proxy, _ := newTestProxy()
	frame := make([]byte, 25)
	binary.LittleEndian.PutUint32(frame[:4], uint32(len(frame)))
	binary.LittleEndian.PutUint32(frame[12:16], opCompressed)
	binary.LittleEndian.PutUint32(frame[16:20], opMsg)
	forward, reply, err := proxy.rewrite(frame)
	require.Error(t, err)
	assert.Nil(t, forward)
	assert.Nil(t, reply)
}

func TestLegacyHandshakeRejectsTruncatedCommands(t *testing.T) {
	frame, docStart := queryHandshake(t, "admin.$cmd", bson.D{{Key: "hello", Value: int32(1)}}, nil)
	badLength := append([]byte(nil), frame...)
	binary.LittleEndian.PutUint32(badLength[docStart:], 0x7fffffff)
	badDocument := append([]byte(nil), frame...)
	badDocument[len(badDocument)-1] = 1
	for name, message := range map[string][]byte{
		"no flags":                frame[:headerSize],
		"no namespace terminator": frame[:headerSize+4+len("admin.$cmd")],
		"no query counts":         frame[:docStart-1],
		"truncated BSON":          frame[:len(frame)-1],
		"invalid length":          badLength,
		"invalid BSON":            badDocument,
	} {
		t.Run(name, func(t *testing.T) {
			proxy, _ := newTestProxy()
			forward, reply, err := proxy.rewrite(message)
			require.ErrorIs(t, err, errMalformed)
			assert.Nil(t, forward)
			assert.Nil(t, reply)
		})
	}
}

func TestPumpNeverForwardsCompressedMessage(t *testing.T) {
	proxy, _ := newTestProxy()
	clientSide, proxyClient := net.Pipe()
	proxyUpstream, upstreamSide := net.Pipe()
	defer func() { _ = clientSide.Close(); _ = upstreamSide.Close() }()
	finished := make(chan struct{})
	forwarded := make(chan []byte, 1)
	go func() {
		bytes, _ := io.ReadAll(upstreamSide)
		forwarded <- bytes
	}()
	go func() {
		defer close(finished)
		defer func() { _ = proxyClient.Close(); _ = proxyUpstream.Close() }()
		proxy.pump(proxyClient, proxyUpstream)
	}()
	frame := make([]byte, 25)
	binary.LittleEndian.PutUint32(frame[:4], uint32(len(frame)))
	binary.LittleEndian.PutUint32(frame[12:16], opCompressed)
	binary.LittleEndian.PutUint32(frame[16:20], opMsg)
	_, err := clientSide.Write(frame)
	require.NoError(t, err)
	_ = clientSide.Close()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("pump did not stop")
	}
	assert.Empty(t, <-forwarded)
}
