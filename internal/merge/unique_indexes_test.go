package merge

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestUniqueIndexSharedPlanKey(t *testing.T) {
	index := uniqueIndex{name: "email_1", paths: [][]string{{"email"}}, owners: make(map[string][]uniqueOwner)}
	report := UniqueCheckResult{}
	doc := func(name string) bson.Raw {
		raw, err := bson.Marshal(bson.D{{Key: "_id", Value: name}, {Key: "email", Value: "shared@example.com"}})
		require.NoError(t, err)
		return raw
	}
	names := []string{"a", "b", "c", "d"}
	for _, name := range names {
		raw := doc(name)
		id := raw.Lookup("_id")
		token, err := uniqueKeyToken([]bson.RawValue{id})
		require.NoError(t, err)
		change := Change{Collection: "users", DocumentRaw: raw}
		require.NoError(t, report.addPlanKey(change, &index, id, token))
	}
	require.Len(t, report.Collisions, 3)
	for i, collision := range report.Collisions {
		require.True(t, collision.InPlan)
		require.Equal(t, strconv.Quote(names[i+1]), string(collision.PlanID))
		require.Equal(t, strconv.Quote(names[0]), string(collision.OtherID))
	}
	require.NoError(t, report.checkSourceDocument("users", doc("existing"), []uniqueIndex{index}, nil))
	require.Len(t, report.Collisions, 7)
	for i, collision := range report.Collisions[3:] {
		require.False(t, collision.InPlan)
		require.Equal(t, strconv.Quote(names[i]), string(collision.PlanID))
		require.Equal(t, `"existing"`, string(collision.OtherID))
	}
}

func TestUniqueCollisionRendersBSONOnOneLine(t *testing.T) {
	raw, err := bson.Marshal(bson.D{
		{Key: "code", Value: primitive.CodeWithScope{Code: "first\nsecond", Scope: bson.D{}}},
		{Key: "_id", Value: "with\ttab"},
	})
	require.NoError(t, err)
	report := UniqueCheckResult{}
	id := bson.Raw(raw).Lookup("_id")
	require.NoError(t, report.addCollision("users", "code_1", raw, id, id, true))
	row := report.Collisions[0]
	require.JSONEq(t, `{"code":{"$code":"first\nsecond","$scope":{}},"_id":"with\ttab"}`, string(row.Key))
	require.NotContains(t, string(row.Key), "\n")
	require.Equal(t, `"with\ttab"`, string(row.PlanID))
}
