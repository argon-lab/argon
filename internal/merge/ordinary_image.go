package merge

import (
	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
)

// Ordinary plans publish the values exposed by materialization. An ordered
// code scope is a scalar here: its encoded fields, including repeats, survive.
func ordinaryImage(raw bson.Raw) (bson.Raw, bool, error) {
	return wal.NormalizeMergeImage(raw)
}
