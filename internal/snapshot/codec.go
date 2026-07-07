package snapshot

import (
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/argon-lab/argon/internal/mongoexpr"
	"github.com/argon-lab/argon/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
)

// The snapshot payload format is a sequence of standard BSON documents,
// each {k: <document ID>, d: <document>}, emitted in sorted-key order so
// identical states always serialize to identical bytes (which is what makes
// chunk-level deduplication effective). The stream is cut into chunks at a
// size threshold and each chunk is compressed independently, so loading
// never needs more than one chunk in memory at a time.

// targetChunkSize is the pre-compression cut-off for a chunk. Kept well
// under MongoDB's 16MB document limit even for incompressible data.
const targetChunkSize = 4 * 1024 * 1024

type frame struct {
	Key string   `bson:"k"`
	Doc bson.Raw `bson:"d"`
}

// encodeState serializes a collection state into compressed chunks.
func encodeState(state map[string]bson.M, compressor *wal.Compressor) (chunks [][]byte, docCount int64, err error) {
	keys := make([]string, 0, len(state))
	for k := range state {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf []byte
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		compressed, err := compressor.Compress(buf)
		if err != nil {
			return fmt.Errorf("failed to compress snapshot chunk: %w", err)
		}
		chunks = append(chunks, compressed)
		buf = nil
		return nil
	}

	for _, k := range keys {
		// Canonicalize before marshalling: documents are Go maps, and
		// marshalling a map serializes keys in randomized order, which
		// would give identical states different bytes — and different
		// chunk hashes — on every run, defeating deduplication.
		canonical, err := mongoexpr.Canonicalize(state[k])
		if err != nil {
			return nil, 0, fmt.Errorf("failed to canonicalize document %s: %w", k, err)
		}
		docRaw, err := bson.Marshal(canonical)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to marshal document %s: %w", k, err)
		}
		frameRaw, err := bson.Marshal(frame{Key: k, Doc: docRaw})
		if err != nil {
			return nil, 0, fmt.Errorf("failed to marshal frame for %s: %w", k, err)
		}
		buf = append(buf, frameRaw...)
		docCount++
		if len(buf) >= targetChunkSize {
			if err := flush(); err != nil {
				return nil, 0, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, 0, err
	}
	return chunks, docCount, nil
}

// decodeChunk appends one decompressed chunk's frames into state.
func decodeChunk(compressed []byte, compressor *wal.Compressor, state map[string]bson.M) error {
	raw, err := compressor.Decompress(compressed)
	if err != nil {
		return fmt.Errorf("failed to decompress snapshot chunk: %w", err)
	}

	// A BSON stream: each document starts with its int32 total length.
	for offset := 0; offset < len(raw); {
		if offset+4 > len(raw) {
			return fmt.Errorf("truncated snapshot chunk at offset %d", offset)
		}
		docLen := int(binary.LittleEndian.Uint32(raw[offset : offset+4]))
		if docLen < 5 || offset+docLen > len(raw) {
			return fmt.Errorf("corrupt snapshot frame at offset %d (len %d)", offset, docLen)
		}
		var f frame
		if err := bson.Unmarshal(raw[offset:offset+docLen], &f); err != nil {
			return fmt.Errorf("failed to decode snapshot frame at offset %d: %w", offset, err)
		}
		var doc bson.M
		if err := bson.Unmarshal(f.Doc, &doc); err != nil {
			return fmt.Errorf("failed to decode document %s: %w", f.Key, err)
		}
		state[f.Key] = doc
		offset += docLen
	}
	return nil
}
