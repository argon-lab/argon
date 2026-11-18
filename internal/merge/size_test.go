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
	plan := &Plan{ID: primitive.NewObjectID(), Status: StatusPending, Changes: []Change{{Document: bson.M{"padding": ""}}}}
	raw, err := bson.Marshal(plan)
	require.NoError(t, err)
	plan.WriteBackLastError = strings.Repeat("e", maxLastErrorBytes)
	failed, err := bson.Marshal(plan)
	require.NoError(t, err)
	plan.WriteBackLastError = ""
	space := maxPlanBytes - len(failed)
	plan.Changes[0].Document["padding"] = strings.Repeat("x", space)
	encoded, err := encodePendingPlan(plan)
	require.NoError(t, err)
	require.Len(t, encoded, len(raw)+space)
	plan.Changes[0].Document["padding"] = strings.Repeat("x", space+1)
	_, err = encodePendingPlan(plan)
	require.ErrorContains(t, err, "16 MiB")
	reason := boundedAuditText(strings.Repeat("é", maxLastErrorBytes), maxLastErrorBytes)
	require.True(t, utf8.ValidString(reason))
	require.LessOrEqual(t, len(reason), maxLastErrorBytes)
	require.True(t, strings.HasSuffix(reason, "…"))
}
