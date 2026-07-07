package mongoexpr

import (
	"bytes"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Package mongoexpr evaluates MongoDB filter and update expressions in
// process. Live traffic never runs through it — applications query and
// write checked-out branches on real mongod. It survives for exactly two
// consumers: the v1→v2 WAL migration, which must resolve legacy expression
// entries one final time, and canonical BSON comparison/serialization
// (canonical.go) used by snapshots and diffs. Replay correctness never
// depends on this code.
//
// Filter support: implicit equality, $eq, $ne, $gt, $gte, $lt, $lte, $in,
// $nin, $exists, $regex, $size, $all, $elemMatch, $and, $or, $nor, $not,
// dotted paths, and match-any-element semantics for arrays. Unsupported
// operators fail loudly instead of being silently skipped.

// MatchesFilter reports whether a document matches a MongoDB query filter.
func MatchesFilter(doc bson.M, filter bson.M) (bool, error) {
	for key, expected := range filter {
		switch key {
		case "$and":
			conds, err := toFilterSlice(expected, "$and")
			if err != nil {
				return false, err
			}
			for _, cond := range conds {
				ok, err := MatchesFilter(doc, cond)
				if err != nil || !ok {
					return false, err
				}
			}
		case "$or":
			conds, err := toFilterSlice(expected, "$or")
			if err != nil {
				return false, err
			}
			matched := false
			for _, cond := range conds {
				ok, err := MatchesFilter(doc, cond)
				if err != nil {
					return false, err
				}
				if ok {
					matched = true
					break
				}
			}
			if !matched {
				return false, nil
			}
		case "$nor":
			conds, err := toFilterSlice(expected, "$nor")
			if err != nil {
				return false, err
			}
			for _, cond := range conds {
				ok, err := MatchesFilter(doc, cond)
				if err != nil {
					return false, err
				}
				if ok {
					return false, nil
				}
			}
		default:
			if strings.HasPrefix(key, "$") {
				return false, fmt.Errorf("unsupported top-level query operator %q", key)
			}
			ok, err := matchesField(doc, key, expected)
			if err != nil || !ok {
				return false, err
			}
		}
	}
	return true, nil
}

// matchesField evaluates a single field condition (dotted paths allowed).
func matchesField(doc bson.M, path string, expected interface{}) (bool, error) {
	value, exists := lookupPath(doc, path)

	if opDoc, ok := asOperatorDoc(expected); ok {
		return matchesOperators(value, exists, opDoc)
	}

	// Implicit equality.
	if !exists {
		return isBSONNull(expected), nil
	}
	return valuesMatch(value, expected), nil
}

// matchesOperators evaluates an operator document like {$gt: 5, $lt: 10}
// against a field value.
func matchesOperators(value interface{}, exists bool, operators bson.M) (bool, error) {
	for op, operand := range operators {
		switch op {
		case "$eq":
			if !exists || !valuesMatch(value, operand) {
				return false, nil
			}
		case "$ne":
			if exists && valuesMatch(value, operand) {
				return false, nil
			}
		case "$gt", "$gte", "$lt", "$lte":
			if !exists || !compareMatch(value, operand, op) {
				return false, nil
			}
		case "$in":
			arr, err := toSlice(operand, "$in")
			if err != nil {
				return false, err
			}
			if !exists {
				return false, nil
			}
			found := false
			for _, item := range arr {
				if valuesMatch(value, item) {
					found = true
					break
				}
			}
			if !found {
				return false, nil
			}
		case "$nin":
			arr, err := toSlice(operand, "$nin")
			if err != nil {
				return false, err
			}
			if exists {
				for _, item := range arr {
					if valuesMatch(value, item) {
						return false, nil
					}
				}
			}
		case "$exists":
			want := toBool(operand)
			if exists != want {
				return false, nil
			}
		case "$regex":
			if !exists {
				return false, nil
			}
			ok, err := regexMatch(value, operand, operators["$options"])
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}
		case "$options":
			// Consumed together with $regex.
		case "$size":
			if !exists {
				return false, nil
			}
			arr, ok := asArray(value)
			if !ok || int64(len(arr)) != toInt64(operand) {
				return false, nil
			}
		case "$all":
			required, err := toSlice(operand, "$all")
			if err != nil {
				return false, err
			}
			if !exists {
				return false, nil
			}
			arr, ok := asArray(value)
			if !ok {
				return false, nil
			}
			for _, req := range required {
				found := false
				for _, item := range arr {
					if compareBSONValues(item, req) == 0 {
						found = true
						break
					}
				}
				if !found {
					return false, nil
				}
			}
		case "$elemMatch":
			if !exists {
				return false, nil
			}
			cond, ok := toBSONM(operand)
			if !ok {
				return false, fmt.Errorf("$elemMatch requires a document operand")
			}
			arr, isArr := asArray(value)
			if !isArr {
				return false, nil
			}
			matched := false
			for _, item := range arr {
				elemDoc, isDoc := toBSONM(item)
				if !isDoc {
					continue
				}
				ok, err := MatchesFilter(elemDoc, cond)
				if err != nil {
					return false, err
				}
				if ok {
					matched = true
					break
				}
			}
			if !matched {
				return false, nil
			}
		case "$not":
			cond, ok := toBSONM(operand)
			if !ok {
				return false, fmt.Errorf("$not requires an operator document")
			}
			ok2, err := matchesOperators(value, exists, cond)
			if err != nil {
				return false, err
			}
			if ok2 {
				return false, nil
			}
		default:
			return false, fmt.Errorf("unsupported query operator %q", op)
		}
	}
	return true, nil
}

// lookupPath resolves a possibly-dotted field path in a document.
func lookupPath(doc bson.M, path string) (interface{}, bool) {
	parts := strings.Split(path, ".")
	var current interface{} = doc
	for _, part := range parts {
		asDoc, ok := toBSONM(current)
		if !ok {
			return nil, false
		}
		next, exists := asDoc[part]
		if !exists {
			return nil, false
		}
		current = next
	}
	return current, true
}

// valuesMatch implements MongoDB equality semantics for filters: direct
// equality, or — when the stored value is an array — equality of any element.
func valuesMatch(value, expected interface{}) bool {
	if compareBSONValues(value, expected) == 0 {
		return true
	}
	if arr, ok := asArray(value); ok {
		for _, item := range arr {
			if compareBSONValues(item, expected) == 0 {
				return true
			}
		}
	}
	return false
}

// compareMatch evaluates $gt/$gte/$lt/$lte with array-any-element semantics.
func compareMatch(value, operand interface{}, op string) bool {
	try := func(v interface{}) bool {
		if !comparableTypes(v, operand) {
			return false
		}
		c := compareBSONValues(v, operand)
		switch op {
		case "$gt":
			return c > 0
		case "$gte":
			return c >= 0
		case "$lt":
			return c < 0
		case "$lte":
			return c <= 0
		}
		return false
	}
	if try(value) {
		return true
	}
	if arr, ok := asArray(value); ok {
		for _, item := range arr {
			if try(item) {
				return true
			}
		}
	}
	return false
}

// compareBSONValues compares two BSON values with numeric cross-type
// promotion. It returns 0 for equal, -1 / 1 for ordering, and a nonzero
// sentinel for incomparable-but-unequal values.
func compareBSONValues(a, b interface{}) int {
	an, aIsNum := toFloat(a)
	bn, bIsNum := toFloat(b)
	if aIsNum && bIsNum {
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		default:
			return 0
		}
	}

	switch av := a.(type) {
	case string:
		if bv, ok := b.(string); ok {
			return strings.Compare(av, bv)
		}
	case bool:
		if bv, ok := b.(bool); ok {
			switch {
			case av == bv:
				return 0
			case !av:
				return -1
			default:
				return 1
			}
		}
	case primitive.ObjectID:
		if bv, ok := b.(primitive.ObjectID); ok {
			return bytes.Compare(av[:], bv[:])
		}
	case primitive.DateTime:
		if bv, ok := b.(primitive.DateTime); ok {
			switch {
			case av < bv:
				return -1
			case av > bv:
				return 1
			default:
				return 0
			}
		}
		if bv, ok := b.(time.Time); ok {
			return compareBSONValues(av.Time().UnixMilli(), bv.UnixMilli())
		}
	case time.Time:
		if bv, ok := b.(time.Time); ok {
			switch {
			case av.Before(bv):
				return -1
			case av.After(bv):
				return 1
			default:
				return 0
			}
		}
		if bv, ok := b.(primitive.DateTime); ok {
			return compareBSONValues(av.UnixMilli(), bv.Time().UnixMilli())
		}
	case nil:
		if b == nil || isBSONNull(b) {
			return 0
		}
	case primitive.Null:
		if b == nil || isBSONNull(b) {
			return 0
		}
	}

	// Documents, arrays and remaining scalar types: equal iff their
	// canonical encodings match. Field order inside documents is ignored
	// (see canonical.go for why).
	if equal, err := CanonicalEqual(a, b); err == nil && equal {
		return 0
	}
	return 2 // Incomparable and not equal.
}

// comparableTypes reports whether ordering between the two values is
// meaningful (same type bracket, in MongoDB terms).
func comparableTypes(a, b interface{}) bool {
	_, aNum := toFloat(a)
	_, bNum := toFloat(b)
	if aNum && bNum {
		return true
	}
	switch a.(type) {
	case string:
		_, ok := b.(string)
		return ok
	case bool:
		_, ok := b.(bool)
		return ok
	case primitive.ObjectID:
		_, ok := b.(primitive.ObjectID)
		return ok
	case primitive.DateTime, time.Time:
		switch b.(type) {
		case primitive.DateTime, time.Time:
			return true
		}
	}
	return false
}

// --- small conversion helpers ---

func toBSONM(v interface{}) (bson.M, bool) {
	switch d := v.(type) {
	case bson.M:
		return d, true
	case map[string]interface{}:
		return bson.M(d), true
	case bson.D:
		m := make(bson.M, len(d))
		for _, e := range d {
			m[e.Key] = e.Value
		}
		return m, true
	default:
		return nil, false
	}
}

// asOperatorDoc reports whether the expected value is an operator document
// like {$gt: 5} rather than a literal document to compare against.
func asOperatorDoc(expected interface{}) (bson.M, bool) {
	doc, ok := toBSONM(expected)
	if !ok || len(doc) == 0 {
		return nil, false
	}
	for k := range doc {
		if !strings.HasPrefix(k, "$") {
			return nil, false
		}
	}
	return doc, true
}

func asArray(v interface{}) ([]interface{}, bool) {
	switch arr := v.(type) {
	case []interface{}:
		return arr, true
	case primitive.A: // bson.A is an alias of primitive.A
		return arr, true
	case []byte:
		// BSON binary, not an array.
		return nil, false
	}
	// Typed Go slices ([]string, []int, ...) reach this in-process path
	// without a BSON round-trip; flatten them via reflection.
	rv := reflect.ValueOf(v)
	if v == nil || (rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array) {
		return nil, false
	}
	out := make([]interface{}, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out, true
}

func toSlice(v interface{}, op string) ([]interface{}, error) {
	if arr, ok := asArray(v); ok {
		return arr, nil
	}
	return nil, fmt.Errorf("%s requires an array operand, got %T", op, v)
}

func toFilterSlice(v interface{}, op string) ([]bson.M, error) {
	arr, ok := asArray(v)
	if !ok {
		return nil, fmt.Errorf("%s requires an array of filters, got %T", op, v)
	}
	filters := make([]bson.M, 0, len(arr))
	for _, item := range arr {
		f, ok := toBSONM(item)
		if !ok {
			return nil, fmt.Errorf("%s elements must be filter documents, got %T", op, item)
		}
		filters = append(filters, f)
	}
	return filters, nil
}

func toFloat(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}

func toInt64(v interface{}) int64 {
	f, _ := toFloat(v)
	return int64(f)
}

func toBool(v interface{}) bool {
	switch b := v.(type) {
	case bool:
		return b
	default:
		f, ok := toFloat(v)
		return ok && f != 0
	}
}

func isBSONNull(v interface{}) bool {
	if v == nil {
		return true
	}
	_, ok := v.(primitive.Null)
	return ok
}

func regexMatch(value, pattern, opts interface{}) (bool, error) {
	str, ok := value.(string)
	if !ok {
		return false, nil
	}

	var expr string
	switch p := pattern.(type) {
	case string:
		expr = p
	case primitive.Regex:
		expr = p.Pattern
		if opts == nil && p.Options != "" {
			opts = p.Options
		}
	default:
		return false, fmt.Errorf("$regex requires a string or regex operand, got %T", pattern)
	}

	if optStr, ok := opts.(string); ok && optStr != "" {
		flags := ""
		for _, o := range optStr {
			switch o {
			case 'i', 'm', 's':
				flags += string(o)
			default:
				return false, fmt.Errorf("unsupported $regex option %q", string(o))
			}
		}
		expr = "(?" + flags + ")" + expr
	}

	re, err := regexp.Compile(expr)
	if err != nil {
		return false, fmt.Errorf("invalid $regex pattern: %w", err)
	}
	return re.MatchString(str), nil
}
