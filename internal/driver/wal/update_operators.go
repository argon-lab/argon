package wal

import (
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// This file implements MongoDB update operator application for the SDK
// write path. Like the filter matcher, it runs exactly once per operation:
// the resulting post-image is logged to the WAL, so replay never re-executes
// any of this.
//
// Supported: $set, $unset, $inc, $mul, $min, $max, $rename, $push (with
// $each), $addToSet (with $each), $pull, $pop, $setOnInsert, $currentDate,
// dotted field paths, and full-document replacement (an update with no $
// operators). Unsupported operators fail loudly.

// ApplyUpdate returns the post-image of applying a MongoDB update document
// to doc. doc is not mutated. isUpsertInsert selects $setOnInsert behavior.
func ApplyUpdate(doc bson.M, update interface{}, isUpsertInsert bool) (bson.M, error) {
	updateDoc, ok := toBSONM(update)
	if !ok {
		return nil, fmt.Errorf("update must be a document, got %T", update)
	}

	result, err := cloneDoc(doc)
	if err != nil {
		return nil, err
	}

	if !hasUpdateOperators(updateDoc) {
		// Replacement semantics: keep _id, take everything else from the
		// replacement document.
		replacement, err := cloneDoc(updateDoc)
		if err != nil {
			return nil, err
		}
		if id, exists := result["_id"]; exists {
			if repID, has := replacement["_id"]; has && compareBSONValues(repID, id) != 0 {
				return nil, fmt.Errorf("the _id field cannot be changed by a replacement")
			}
			replacement["_id"] = id
		}
		return replacement, nil
	}

	for op, operand := range updateDoc {
		fields, ok := toBSONM(operand)
		if !ok {
			return nil, fmt.Errorf("%s requires a document operand, got %T", op, operand)
		}
		for path, value := range fields {
			if err := applyUpdateOperator(result, op, path, value, isUpsertInsert); err != nil {
				return nil, err
			}
		}
	}

	if id, exists := doc["_id"]; exists {
		if newID, has := result["_id"]; !has || compareBSONValues(newID, id) != 0 {
			return nil, fmt.Errorf("the _id field cannot be changed by an update")
		}
	}

	return result, nil
}

// BuildUpsertDocument constructs the document an upsert inserts when no
// document matched: the equality conditions of the filter, plus the update
// applied with $setOnInsert active.
func BuildUpsertDocument(filter bson.M, update interface{}) (bson.M, error) {
	seed := bson.M{}
	for path, expected := range filter {
		if strings.HasPrefix(path, "$") {
			continue // $and/$or conditions don't seed upserts here.
		}
		if opDoc, isOp := asOperatorDoc(expected); isOp {
			if eq, has := opDoc["$eq"]; has {
				setPath(seed, path, eq)
			}
			continue
		}
		setPath(seed, path, expected)
	}
	return ApplyUpdate(seed, update, true)
}

func applyUpdateOperator(doc bson.M, op, path string, value interface{}, isUpsertInsert bool) error {
	switch op {
	case "$set":
		setPath(doc, path, value)
	case "$setOnInsert":
		if isUpsertInsert {
			setPath(doc, path, value)
		}
	case "$unset":
		unsetPath(doc, path)
	case "$inc", "$mul":
		operand, ok := toFloat(value)
		if !ok {
			return fmt.Errorf("%s requires a numeric operand for field %s", op, path)
		}
		current, exists := lookupPath(doc, path)
		if !exists {
			if op == "$inc" {
				setPath(doc, path, value)
			} else {
				// $mul on a missing field sets it to zero of the operand type.
				setPath(doc, path, zeroLike(value))
			}
			return nil
		}
		curNum, ok := toFloat(current)
		if !ok {
			return fmt.Errorf("cannot apply %s to non-numeric field %s", op, path)
		}
		var result float64
		if op == "$inc" {
			result = curNum + operand
		} else {
			result = curNum * operand
		}
		setPath(doc, path, promoteNumeric(current, value, result))
	case "$min", "$max":
		current, exists := lookupPath(doc, path)
		if !exists {
			setPath(doc, path, value)
			return nil
		}
		c := compareBSONValues(value, current)
		if c == 2 {
			return nil // Incomparable types: MongoDB uses type ordering; keep current for supported set.
		}
		if (op == "$min" && c < 0) || (op == "$max" && c > 0) {
			setPath(doc, path, value)
		}
	case "$rename":
		newPath, ok := value.(string)
		if !ok {
			return fmt.Errorf("$rename requires a string target for field %s", path)
		}
		if current, exists := lookupPath(doc, path); exists {
			unsetPath(doc, path)
			setPath(doc, newPath, current)
		}
	case "$push":
		return applyPush(doc, path, value)
	case "$addToSet":
		return applyAddToSet(doc, path, value)
	case "$pull":
		return applyPull(doc, path, value)
	case "$pop":
		return applyPop(doc, path, value)
	case "$currentDate":
		setPath(doc, path, time.Now().UTC())
	default:
		return fmt.Errorf("unsupported update operator %q", op)
	}
	return nil
}

func applyPush(doc bson.M, path string, value interface{}) error {
	items := []interface{}{value}
	if mod, isMod := asOperatorDoc(value); isMod {
		each, has := mod["$each"]
		if !has {
			return fmt.Errorf("$push modifiers other than $each are not supported")
		}
		if len(mod) > 1 {
			return fmt.Errorf("$push modifiers other than $each are not supported")
		}
		arr, ok := asArray(each)
		if !ok {
			return fmt.Errorf("$each requires an array")
		}
		items = arr
	}

	current, exists := lookupPath(doc, path)
	if !exists {
		setPath(doc, path, append([]interface{}{}, items...))
		return nil
	}
	arr, ok := asArray(current)
	if !ok {
		return fmt.Errorf("cannot $push to non-array field %s", path)
	}
	setPath(doc, path, append(append([]interface{}{}, arr...), items...))
	return nil
}

func applyAddToSet(doc bson.M, path string, value interface{}) error {
	items := []interface{}{value}
	if mod, isMod := asOperatorDoc(value); isMod {
		each, has := mod["$each"]
		if !has || len(mod) > 1 {
			return fmt.Errorf("$addToSet modifiers other than $each are not supported")
		}
		arr, ok := asArray(each)
		if !ok {
			return fmt.Errorf("$each requires an array")
		}
		items = arr
	}

	var arr []interface{}
	if current, exists := lookupPath(doc, path); exists {
		existing, ok := asArray(current)
		if !ok {
			return fmt.Errorf("cannot $addToSet to non-array field %s", path)
		}
		arr = append(arr, existing...)
	}

	for _, item := range items {
		present := false
		for _, existing := range arr {
			if compareBSONValues(existing, item) == 0 {
				present = true
				break
			}
		}
		if !present {
			arr = append(arr, item)
		}
	}
	setPath(doc, path, arr)
	return nil
}

func applyPull(doc bson.M, path string, condition interface{}) error {
	current, exists := lookupPath(doc, path)
	if !exists {
		return nil
	}
	arr, ok := asArray(current)
	if !ok {
		return fmt.Errorf("cannot $pull from non-array field %s", path)
	}

	kept := make([]interface{}, 0, len(arr))
	for _, item := range arr {
		remove := false
		if opDoc, isOp := asOperatorDoc(condition); isOp {
			ok, err := matchesOperators(item, true, opDoc)
			if err != nil {
				return err
			}
			remove = ok
		} else if condDoc, isDoc := toBSONM(condition); isDoc {
			itemDoc, isItemDoc := toBSONM(item)
			if isItemDoc {
				ok, err := MatchesFilter(itemDoc, condDoc)
				if err != nil {
					return err
				}
				remove = ok
			}
		} else {
			remove = compareBSONValues(item, condition) == 0
		}
		if !remove {
			kept = append(kept, item)
		}
	}
	setPath(doc, path, kept)
	return nil
}

func applyPop(doc bson.M, path string, value interface{}) error {
	current, exists := lookupPath(doc, path)
	if !exists {
		return nil
	}
	arr, ok := asArray(current)
	if !ok {
		return fmt.Errorf("cannot $pop from non-array field %s", path)
	}
	if len(arr) == 0 {
		return nil
	}
	switch toInt64(value) {
	case 1:
		setPath(doc, path, append([]interface{}{}, arr[:len(arr)-1]...))
	case -1:
		setPath(doc, path, append([]interface{}{}, arr[1:]...))
	default:
		return fmt.Errorf("$pop requires 1 or -1")
	}
	return nil
}

// hasUpdateOperators reports whether the update document uses operator form.
// MongoDB rejects mixed operator/literal documents; so do we, implicitly,
// because a leading non-$ key selects replacement semantics.
func hasUpdateOperators(update bson.M) bool {
	for k := range update {
		if strings.HasPrefix(k, "$") {
			return true
		}
	}
	return false
}

// cloneDoc deep-copies a document through BSON marshalling, which also
// normalizes nested maps to bson.M.
func cloneDoc(doc bson.M) (bson.M, error) {
	if doc == nil {
		return bson.M{}, nil
	}
	raw, err := bson.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("failed to clone document: %w", err)
	}
	var clone bson.M
	if err := bson.Unmarshal(raw, &clone); err != nil {
		return nil, fmt.Errorf("failed to clone document: %w", err)
	}
	return clone, nil
}

// promoteNumeric picks the result type for $inc/$mul following MongoDB's
// promotion rules: any double operand makes the result a double, otherwise
// any int64 makes it an int64, otherwise it stays int32.
func promoteNumeric(current, operand interface{}, result float64) interface{} {
	if isFloatKind(current) || isFloatKind(operand) {
		return result
	}
	if isInt64Kind(current) || isInt64Kind(operand) {
		return int64(result)
	}
	return int32(result)
}

func isFloatKind(v interface{}) bool {
	switch v.(type) {
	case float32, float64:
		return true
	}
	return false
}

func isInt64Kind(v interface{}) bool {
	switch v.(type) {
	case int64, int:
		return true
	}
	return false
}

func zeroLike(v interface{}) interface{} {
	if isFloatKind(v) {
		return float64(0)
	}
	if isInt64Kind(v) {
		return int64(0)
	}
	return int32(0)
}

// setPath sets a (possibly dotted) field path, creating intermediate
// documents as needed.
func setPath(doc bson.M, path string, value interface{}) {
	parts := strings.Split(path, ".")
	current := doc
	for _, part := range parts[:len(parts)-1] {
		next, exists := current[part]
		if exists {
			if nextDoc, ok := toBSONM(next); ok {
				current[part] = nextDoc
				current = nextDoc
				continue
			}
		}
		created := bson.M{}
		current[part] = created
		current = created
	}
	current[parts[len(parts)-1]] = value
}

// unsetPath removes a (possibly dotted) field path if it exists.
func unsetPath(doc bson.M, path string) {
	parts := strings.Split(path, ".")
	current := doc
	for _, part := range parts[:len(parts)-1] {
		next, exists := current[part]
		if !exists {
			return
		}
		nextDoc, ok := toBSONM(next)
		if !ok {
			return
		}
		current[part] = nextDoc
		current = nextDoc
	}
	delete(current, parts[len(parts)-1])
}
