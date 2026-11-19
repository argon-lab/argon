package merge

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestPlanFitsThroughFailedAttempt(t *testing.T) {
	plan := &Plan{
		ID: primitive.NewObjectID(), FormatVersion: orderedPlanVersion, Status: StatusPending,
		Changes: []Change{{SortedImages: []string{"document"}}},
	}
	setPadding := func(n int) {
		t.Helper()
		raw, err := bson.Marshal(bson.D{{Key: "padding", Value: strings.Repeat("x", n)}})
		require.NoError(t, err)
		plan.Changes[0].DocumentRaw = raw
	}
	setPadding(0)
	raw, err := bson.Marshal(plan)
	require.NoError(t, err)
	plan.WriteBackLastError = strings.Repeat("e", maxLastErrorBytes)
	failed, err := bson.Marshal(plan)
	require.NoError(t, err)
	plan.WriteBackLastError = ""
	space := maxPlanBytes - len(failed)
	setPadding(space)
	encoded, err := encodePendingPlan(plan)
	require.NoError(t, err)
	require.Len(t, encoded, len(raw)+space)
	setPadding(space + 1)
	_, err = encodePendingPlan(plan)
	require.ErrorContains(t, err, "16 MiB")
	reason := boundedAuditText(strings.Repeat("é", maxLastErrorBytes), maxLastErrorBytes)
	require.True(t, utf8.ValidString(reason))
	require.LessOrEqual(t, len(reason), maxLastErrorBytes)
	require.True(t, strings.HasSuffix(reason, "…"))
}
