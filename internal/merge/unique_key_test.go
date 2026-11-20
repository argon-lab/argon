package merge

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestUniqueKeyToken(t *testing.T) {
	decimal := func(s string) primitive.Decimal128 {
		d, err := primitive.ParseDecimal128(s)
		require.NoError(t, err)
		return d
	}
	token := func(values ...any) string {
		var parts []bson.RawValue
		for _, v := range values {
			raw, err := bson.Marshal(bson.D{{Key: "v", Value: v}})
			require.NoError(t, err)
			parts = append(parts, bson.Raw(raw).Lookup("v"))
		}
		key, err := uniqueKeyToken(parts)
		require.NoError(t, err)
		return key
	}

	for _, group := range [][]any{
		{int32(5), int64(5), float64(5), decimal("5.000")},
		{decimal("-1.1250"), -1.125},
		{decimal("-0E-2000"), math.Copysign(0, -1), int32(0), primitive.NewDecimal128(0x3041ffffffffffff, math.MaxUint64)},
		{decimal("-Infinity"), math.Inf(-1)},
		{decimal("NaN"), math.NaN(), math.Float64frombits(0x7ff0000000000001)},
		{"a\x00b", primitive.Symbol("a\x00b")},
		{bson.D{{Key: "v", Value: bson.A{decimal("1")}}}, bson.D{{Key: "v", Value: bson.A{1.0}}}},
		{primitive.CodeWithScope{Code: "v", Scope: bson.D{{Key: "v", Value: 4.0}}},
			primitive.CodeWithScope{Code: "v", Scope: bson.D{{Key: "v", Value: decimal("4")}}}},
	} {
		first := token(group[0])
		for _, other := range group[1:] {
			require.Equal(t, first, token(other), "%#v and %#v", group[0], other)
		}
	}

	for _, pair := range [][2]any{
		{0.1, decimal("0.1")},
		{float64(1 << 53), int64(1<<53 + 1)},
		{decimal("1E-6176"), 0.0},
		{math.Inf(1), decimal("1E6111")},
		{int32(1), "1"},
		{nil, primitive.Undefined{}},
		{bson.A{"x"}, bson.D{{Key: "0", Value: "x"}}},
		{bson.D{{Key: "a", Value: 1}, {Key: "b", Value: 2}}, bson.D{{Key: "b", Value: 2}, {Key: "a", Value: 1}}},
	} {
		require.NotEqual(t, token(pair[0]), token(pair[1]), "%#v and %#v", pair[0], pair[1])
	}
	require.NotEqual(t, token("a", "bc"), token("ab", "c"))
}
