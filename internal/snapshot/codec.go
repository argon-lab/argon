package snapshot

import (
	"context"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/argon-lab/argon/v2/internal/wal"
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
func encodeState(state map[string]bson.Raw, compressor *wal.Compressor) (chunks [][]byte, docCount int64, err error) {
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
		frameRaw, err := bson.Marshal(frame{Key: k, Doc: state[k]})
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

// walkChunk borrows each frame's key and document directly from the BSON stream.
func walkChunk(ctx context.Context, compressed []byte, compressor *wal.Compressor, visit func(frame) error) error {
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
		if err := ctx.Err(); err != nil {
			return err
		}
		rawFrame := bson.Raw(raw[offset : offset+docLen])
		key, ok := rawFrame.Lookup("k").StringValueOK()
		doc, docOK := rawFrame.Lookup("d").DocumentOK()
		if !ok || !docOK {
			return fmt.Errorf("invalid snapshot frame at offset %d", offset)
		}
		if err := visit(frame{Key: key, Doc: doc}); err != nil {
			return err
		}
		offset += docLen
	}
	return nil
}

// decoded supplies the key and value used by every snapshot replay.
func (f frame) decoded() (string, *wal.DecodedImage, error) {
	doc, err := wal.DecodeImage(f.Doc)
	if err != nil {
		return "", nil, fmt.Errorf("failed to decode document %s: %w", f.Key, err)
	}
	// Older writers conflated ObjectIDs with equal-looking strings.
	key, idErr := doc.Key()
	if idErr != nil {
		key = f.Key
	}
	return key, doc, nil
}

func decodeChunk(ctx context.Context, compressed []byte, compressor *wal.Compressor, state map[string]bson.M) error {
	return walkChunk(ctx, compressed, compressor, func(f frame) error {
		key, doc, err := f.decoded()
		if err == nil {
			state[key] = doc.TakeValue()
		}
		return err
	})
}
