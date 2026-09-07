package merge

import (
	"reflect"
	"testing"

	"github.com/argon-lab/argon/internal/wal"
)

func TestDeleteKeysRoundTrip(t *testing.T) {
	for _, id := range []interface{}{int32(42), int64(42), "507f1f77bcf86cd799439011"} {
		got, err := documentIDValue(Change{DocumentID: wal.DocumentIDString(id), Delete: true})
		if err != nil || !reflect.DeepEqual(got, id) {
			t.Fatalf("%T(%v) became %T(%v), err=%v", id, id, got, got, err)
		}
	}
}
