package checkout

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/mongo"
)

// A checkout that Atlas refuses on a tier limit says why and what to do,
// and keeps Atlas's own message with its numbers.
func TestExplainAtlasRestriction(t *testing.T) {
	limit := mongo.CommandError{Code: 8000, Name: "AtlasError", Message: "cannot create a new collection -- already using 500 collections of 500"}
	err := explainAtlasRestriction(fmt.Errorf("collection orders: %w", limit))
	for _, want := range []string{"already using 500 collections of 500", "tier limit", "100 databases", "500 collections", "0.5 GB", "argon sandbox discard", "dedicated tier"} {
		require.Contains(t, err.Error(), want)
	}
	var original mongo.CommandError
	require.True(t, errors.As(err, &original), "Atlas's error stays wrapped")

	other := errors.New("failed to materialize collection orders")
	require.Same(t, other, explainAtlasRestriction(other))
	privilege := mongo.CommandError{Code: 8000, Name: "AtlasError", Message: "user is not allowed to do action [insert] on [db.c]"}
	require.False(t, strings.Contains(explainAtlasRestriction(privilege).Error(), "tier limit"))
}
