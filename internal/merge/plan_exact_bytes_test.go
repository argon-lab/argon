package merge

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/x/bsonx/bsoncore"
)

type jsonExactValue struct {
	Type string
	Hex  string
}

type jsonExactChange struct {
	Document json.RawMessage
	Exact    map[string]jsonExactValue `json:"exact_bytes"`
	Literals []string                  `json:"literal_dollar_paths"`
}

func jsonImage(t *testing.T, value any) bson.Raw {
	t.Helper()
	raw, err := bson.Marshal(bson.D{{Key: "v", Value: value}})
	require.NoError(t, err)
	return raw
}

func renderExactChange(t *testing.T, image bson.Raw) jsonExactChange {
	t.Helper()
	p := Plan{FormatVersion: orderedPlanVersion, Changes: []Change{{DocumentID: "x", DocumentRaw: image}}}
	data, err := p.CLIJSON(false)
	require.NoError(t, err)
	var result struct{ Changes []jsonExactChange }
	require.NoError(t, json.Unmarshal(data, &result))
	require.Len(t, result.Changes, 1)
	change := result.Changes[0]
	count, err := p.ExactBytePathCount()
	require.NoError(t, err)
	require.Equal(t, len(change.Exact), count)
	return change
}

func TestPlanJSONExactBytesByType(t *testing.T) {
	// Use the parser as an independent oracle. Equal JSON strings (including
	// different spellings of U+FFFD) must identify the same original bytes.
	tags := map[byte]bool{}
	visible := map[string]bson.Raw{}
	check := func(value any) {
		t.Helper()
		raw := jsonImage(t, value)
		v := raw.Lookup("v")
		tags[byte(v.Type)] = true
		change := renderExactChange(t, raw)
		var restored bson.Raw
		err := bson.UnmarshalExtJSON(change.Document, true, &restored)
		if err != nil || !bytes.Equal(raw, restored) {
			want := jsonExactValue{fmt.Sprintf("%02x", byte(v.Type)), hex.EncodeToString(v.Value)}
			require.Equal(t, map[string]jsonExactValue{"document.v": want}, change.Exact, "value %x", v.Value)
		} else {
			require.Nil(t, change.Exact, "value %x", v.Value)
		}
		var obj any
		require.NoError(t, json.Unmarshal(change.Document, &obj))
		signature, err := json.Marshal([]any{obj, change.Literals, change.Exact})
		require.NoError(t, err)
		if previous, ok := visible[string(signature)]; ok {
			require.Equal(t, previous, raw, "identical JSON for distinct BSON %s", signature)
		}
		visible[string(signature)] = raw
	}

	for _, v := range []any{
		nil, primitive.Undefined{}, primitive.MinKey{}, primitive.MaxKey{},
		true, false, primitive.NilObjectID, bson.D{}, bson.A{},
		int32(math.MinInt32), int32(math.MaxInt32), int64(math.MinInt64), int64(math.MaxInt64),
		int32(1), int64(1), float64(1), float64(0), math.Copysign(0, -1), math.SmallestNonzeroFloat64, math.MaxFloat64,
		math.Inf(-1), math.Inf(1), math.NaN(), math.Float64frombits(0x7ff0000000000001),
		math.Float64frombits(0x7ff8000000000002), math.Float64frombits(0xfff8000000000001),
		primitive.DateTime(math.MinInt64), primitive.DateTime(math.MaxInt64),
		primitive.Timestamp{}, primitive.Timestamp{T: math.MaxUint32, I: math.MaxUint32},
		bson.RawValue{Type: bson.TypeArray, Value: bsoncore.BuildDocument(nil, bsoncore.AppendInt32Element(nil, "2", 1))},
		bson.A{int32(1)}, bson.D{{Key: "0", Value: int32(1)}},
	} {
		check(v)
	}
	for _, d := range []string{"0", "-0", "0.00", "-0E-3000", "1", "1.00", "1E-6176", "1E+6111", "NaN", "Infinity", "-Infinity"} {
		decimal, err := primitive.ParseDecimal128(d)
		require.NoError(t, err)
		check(decimal)
	}
	code := bsoncore.AppendCodeWithScope(nil, "x", bsoncore.BuildDocument(nil))
	code[9] = '!'
	check(bson.RawValue{Type: bson.TypeCodeWithScope, Value: code})
	for n := 0; n <= 4; n++ {
		// Short old binaries without an inner length are also readable.
		binary := append([]byte{byte(n), 0, 0, 0, 2}, make([]byte, n)...)
		check(bson.RawValue{Type: bson.TypeBinary, Value: binary})
	}
	for _, d := range [][2]uint64{
		{0x7c00000000000000, 1}, {0xfc00000000000000, 0}, {0x7e00000000000000, 0},
		{0x7800000000000000, 1}, {0xf800000000000000, 123},
		{0x6000000000000000, 1}, {0x3041ffffffffffff, math.MaxUint64},
	} {
		check(primitive.NewDecimal128(d[0], d[1]))
	}
	for _, s := range []string{"", "\uFFFD", "\xff", "\xff\xfe", "a\x00b", "\t\n\u2028\\\""} {
		check(s)
		check(primitive.Symbol(s))
		check(primitive.JavaScript(s))
		check(primitive.DBPointer{DB: s})
		check(primitive.CodeWithScope{Code: primitive.JavaScript(s), Scope: bson.D{}})
	}
	for _, pattern := range []string{"test", "\xff", "\uFFFD"} {
		for _, options := range []string{"", "im", "mi", "ii", "\xff", "\uFFFD"} {
			check(bson.RawValue{Type: bson.TypeRegex, Value: bsoncore.AppendRegex(nil, pattern, options)})
		}
	}
	for subtype := 0; subtype <= math.MaxUint8; subtype++ {
		for _, payload := range [][]byte{nil, {1, 2, 3}, {0, 0, 0, 0}} {
			check(primitive.Binary{Subtype: byte(subtype), Data: payload})
		}
	}
	rng := rand.New(rand.NewPCG(372, 434))
	for range 128 {
		bits := rng.Uint64()
		id := primitive.ObjectID{}
		for i := range id {
			id[i] = byte(rng.Uint32())
		}
		for _, v := range []any{
			id, int32(bits), int64(bits), math.Float64frombits(bits),
			math.Float64frombits(0x7ff8000000000000 | (bits & 0x8007ffffffffffff)),
			primitive.NewDecimal128(bits, rng.Uint64()), primitive.DateTime(int64(bits)),
			primitive.Timestamp{T: uint32(bits), I: uint32(bits >> 32)},
			string(id[:]), primitive.Symbol(string(id[:])), primitive.JavaScript(string(id[:])),
		} {
			check(v)
		}
	}
	require.Len(t, tags, 21)
}

func TestPlanJSONExactBytesPaths(t *testing.T) {
	nan := math.Float64frombits(0x7ff8000000000042)
	image := jsonImage(t, bson.D{{Key: `a.b\c`, Value: bson.A{
		primitive.CodeWithScope{Code: "x", Scope: bson.D{{Key: "$numberDouble", Value: nan}}},
	}}})
	p := Plan{
		FormatVersion: orderedPlanVersion,
		Changes:       []Change{{DocumentID: "x", DocumentRaw: image, BeforeRaw: image}},
		Conflicts:     []Conflict{{DocumentID: "x", BaseRaw: image, OursRaw: image, TheirsRaw: image}},
	}
	data, err := p.CLIJSON(false)
	require.NoError(t, err)
	var result struct{ Changes, Conflicts []jsonExactChange }
	require.NoError(t, json.Unmarshal(data, &result))
	for i, item := range []jsonExactChange{result.Changes[0], result.Conflicts[0]} {
		names := [][]string{{"document", "before"}, {"base", "ours", "theirs"}}[i]
		require.Len(t, item.Exact, len(names))
		for _, name := range names {
			scope := name + `.v.a\.b\\c.0.$scope`
			require.Contains(t, item.Literals, scope)
			require.Equal(t, jsonExactValue{"01", "420000000000f87f"}, item.Exact[scope+".$numberDouble"])
		}
	}
	count, err := p.ExactBytePathCount()
	require.NoError(t, err)
	require.Equal(t, 5, count)

	dup := bson.D{{Key: "x", Value: 1}, {Key: "x", Value: 2}}
	badNames := []any{dup, bson.A{dup}, primitive.CodeWithScope{Code: "x", Scope: dup},
		bson.D{{Key: "\xff", Value: 1}}, bson.A{bson.D{{Key: "\xff", Value: 1}}},
		bson.RawValue{Type: bson.TypeArray, Value: bsoncore.BuildDocument(nil,
			bsoncore.AppendInt32Element(nil, "0", 1), bsoncore.AppendInt32Element(nil, "0", 2))},
	}
	for _, value := range badNames {
		raw := jsonImage(t, value)
		change := renderExactChange(t, raw)
		require.Equal(t, map[string]jsonExactValue{"document": {"03", hex.EncodeToString(raw)}}, change.Exact)
	}
	// The inner length is short. The rest is still part of this value, so
	// it cannot become another field in the document's JSON.
	payload := bsoncore.AppendInt32Element([]byte{42}, "extra", 7)
	binary := bsoncore.AppendBinary(nil, 2, payload)
	binary[5] = 1
	raw := jsonImage(t, bson.RawValue{Type: bson.TypeBinary, Value: binary})
	change := renderExactChange(t, raw)
	require.NotContains(t, string(change.Document), `"extra"`)
	require.Equal(t, map[string]jsonExactValue{"document": {"03", hex.EncodeToString(raw)}}, change.Exact)
}

func TestPlanJSONLiteralWrappersWithExactBytes(t *testing.T) {
	// The scalar's options and a literal string field look alike in JSON,
	// even when both need their original bytes to distinguish invalid text.
	left := jsonImage(t, bson.RawValue{Type: bson.TypeRegex, Value: bsoncore.AppendRegex(nil, "p", "\xff")})
	right := jsonImage(t, bson.D{{Key: "$regularExpression", Value: bson.D{
		{Key: "pattern", Value: "p"}, {Key: "options", Value: "\xff"},
	}}})
	scalar, literal := renderExactChange(t, left), renderExactChange(t, right)
	require.JSONEq(t, string(scalar.Document), string(literal.Document))
	require.Empty(t, scalar.Literals)
	require.Equal(t, []string{"document.v"}, literal.Literals)
	require.Contains(t, scalar.Exact, "document.v")
	require.Contains(t, literal.Exact, "document.v.$regularExpression.options")
}

func TestPlanJSONStoredLegacyImages(t *testing.T) {
	// A format-2 image could be saved with a different member order from its
	// typed document key. Listing exposes both pieces of information.
	key := bson.D{{Key: "z", Value: 1}, {Key: "a", Value: 2}}
	stored, err := bson.Marshal(bson.D{
		{Key: "u", Value: "\xff"}, {Key: "_id", Value: bson.D{key[1], key[0]}},
	})
	require.NoError(t, err)
	plan := Plan{FormatVersion: 2, Changes: []Change{{
		DocumentID: wal.DocumentIDString(key), DocumentRaw: stored,
	}}}
	data, err := plan.CLIJSON(false)
	require.NoError(t, err)
	var result struct {
		Changes []struct {
			jsonExactChange
			ID json.RawMessage `json:"document_id"`
		}
	}
	require.NoError(t, json.Unmarshal(data, &result))
	c := result.Changes[0]
	require.Equal(t, `{"z":{"$numberInt":"1"},"a":{"$numberInt":"2"}}`, string(c.ID))
	require.Contains(t, string(c.Document), `"_id":{"a":{"$numberInt":"2"},"z":{"$numberInt":"1"}}`)
	require.Contains(t, c.Exact, "document.u")
}
