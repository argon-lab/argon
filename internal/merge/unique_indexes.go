package merge

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

type uniqueIndex struct {
	name   string
	paths  [][]string
	sparse bool
	owners map[string][]uniqueOwner
	// Several documents can encounter the same unsupported key path.
	skipped map[string]bool
}

type uniqueOwner struct {
	id      bson.RawValue
	idToken string
	key     bson.Raw
}

func checkUniqueIndexes(ctx context.Context, database *mongo.Database, changes []Change) (*UniqueCheckResult, error) {
	collections := make(map[string][]Change)
	for _, change := range changes {
		collections[change.Collection] = append(collections[change.Collection], change)
	}
	result := &UniqueCheckResult{Collisions: []UniqueCollision{}, NotChecked: []UncheckedUniqueIndex{}}
	for _, name := range slices.Sorted(maps.Keys(collections)) {
		changes := collections[name]
		if !slices.ContainsFunc(changes, func(c Change) bool { return !c.Delete }) {
			continue
		}
		coll := database.Collection(name, options.Collection().SetReadPreference(readpref.Primary()))
		if err := result.checkCollection(ctx, coll, changes); err != nil {
			return nil, fmt.Errorf("checking unique indexes on %s: %w", name, err)
		}
	}
	return result, nil
}

func (r *UniqueCheckResult) readIndexes(ctx context.Context, coll *mongo.Collection) ([]uniqueIndex, error) {
	cursor, err := coll.Indexes().List(ctx)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var indexes []uniqueIndex
	for cursor.Next(ctx) {
		var spec struct {
			Name      string
			Key       bson.Raw
			Unique    bool
			Sparse    bool
			Partial   bson.RawValue `bson:"partialFilterExpression"`
			Collation bson.RawValue
		}
		if err := cursor.Decode(&spec); err != nil {
			return nil, err
		}
		if !spec.Unique {
			continue
		}
		fields, err := spec.Key.Elements()
		if err != nil {
			return nil, err
		}
		if len(fields) == 1 && fields[0].Key() == "_id" {
			continue
		}
		index := uniqueIndex{
			name: spec.Name, sparse: spec.Sparse,
			owners: make(map[string][]uniqueOwner), skipped: make(map[string]bool),
		}
		reason := ""
		switch {
		case !spec.Partial.IsZero():
			reason = "partial index"
		case !spec.Collation.IsZero():
			reason = "index has a collation"
		default:
			for _, field := range fields {
				path := strings.Split(field.Key(), ".")
				if path[len(path)-1] == "$**" {
					reason = "wildcard index"
					break
				}
				if kind, ok := field.Value().StringValueOK(); ok {
					reason = kind + " index"
					break
				}
				if !field.Value().IsNumber() {
					reason = "unsupported index key pattern"
					break
				}
				index.paths = append(index.paths, path)
			}
		}
		if reason != "" {
			r.NotChecked = append(r.NotChecked, UncheckedUniqueIndex{coll.Name(), spec.Name, reason})
			continue
		}
		indexes = append(indexes, index)
	}
	return indexes, cursor.Err()
}

func (r *UniqueCheckResult) checkCollection(ctx context.Context, coll *mongo.Collection, changes []Change) error {
	indexes, err := r.readIndexes(ctx, coll)
	if err != nil || len(indexes) == 0 {
		return err
	}
	// A delete or replacement can release an existing source key.
	modified := make(map[string]bool)
	for _, change := range changes {
		id, err := wal.RawDocumentID(change.DocumentID)
		if err != nil {
			return err
		}
		idToken, err := uniqueKeyToken([]bson.RawValue{id})
		if err != nil {
			return err
		}
		if change.Delete || change.BeforeRaw != nil {
			modified[idToken] = true
		}
		if change.Delete {
			continue
		}
		for i := range indexes {
			if err := r.addPlanKey(change, &indexes[i], id, idToken); err != nil {
				return err
			}
		}
	}
	indexes = slices.DeleteFunc(indexes, func(index uniqueIndex) bool { return len(index.owners) == 0 })
	if len(indexes) == 0 {
		return nil
	}

	// Read each source document once. Project whole top-level fields so an array
	// encountered anywhere along a dotted path remains visible.
	projection := bson.M{"_id": 1}
	for _, index := range indexes {
		for _, path := range index.paths {
			projection[path[0]] = 1
		}
	}
	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetProjection(projection))
	if err != nil {
		return err
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		if err := r.checkSourceDocument(coll.Name(), cursor.Current, indexes, modified); err != nil {
			return err
		}
	}
	return cursor.Err()
}

func (r *UniqueCheckResult) addPlanKey(change Change, index *uniqueIndex, id bson.RawValue, idToken string) error {
	values := r.keyValues(change.Collection, index, change.DocumentRaw)
	if values == nil {
		return nil
	}
	token, err := uniqueKeyToken(values)
	if err != nil {
		return err
	}
	key := make(bson.D, len(values))
	for i, value := range values {
		key[i] = bson.E{Key: strings.Join(index.paths[i], "."), Value: value}
	}
	raw, err := bson.Marshal(key)
	if err != nil {
		return err
	}
	// Pair each additional owner with the first so the report names every
	// document and grows linearly even when many documents share one key.
	owners := index.owners[token]
	if len(owners) > 0 {
		if err := r.addCollision(change.Collection, index.name, raw, id, owners[0].id, true); err != nil {
			return err
		}
	}
	index.owners[token] = append(owners, uniqueOwner{id, idToken, raw})
	return nil
}

func (r *UniqueCheckResult) checkSourceDocument(collection string, doc bson.Raw, indexes []uniqueIndex, modified map[string]bool) error {
	id, err := doc.LookupErr("_id")
	if err != nil {
		return err
	}
	for i := range indexes {
		index := &indexes[i]
		values := r.keyValues(collection, index, doc)
		if values == nil {
			continue
		}
		token, err := uniqueKeyToken(values)
		if err != nil {
			return fmt.Errorf("source key for index %s: %w", index.name, err)
		}
		owners := index.owners[token]
		if len(owners) == 0 {
			continue
		}
		idToken, err := uniqueKeyToken([]bson.RawValue{id})
		if err != nil {
			return fmt.Errorf("source _id for index %s: %w", index.name, err)
		}
		if modified[idToken] && !slices.ContainsFunc(owners, func(o uniqueOwner) bool { return o.idToken == idToken }) {
			continue
		}
		for _, owner := range owners {
			if owner.idToken == idToken {
				continue
			}
			if err := r.addCollision(collection, index.name, owner.key, owner.id, id, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *UniqueCheckResult) keyValues(collection string, index *uniqueIndex, doc bson.Raw) []bson.RawValue {
	values := make([]bson.RawValue, len(index.paths))
	present := false
	for i, path := range index.paths {
		value := uniquePathValue(doc, path)
		switch value.Type {
		case bson.TypeArray:
			reason := fmt.Sprintf("array on indexed path %q", strings.Join(path, "."))
			if !index.skipped[reason] {
				r.NotChecked = append(r.NotChecked, UncheckedUniqueIndex{collection, index.name, reason})
				index.skipped[reason] = true
			}
			return nil
		case 0:
			values[i] = bson.RawValue{Type: bson.TypeNull}
		default:
			values[i] = value
			present = true
		}
	}
	if index.sparse && !present {
		return nil
	}
	return values
}

// Return the array even when it occurs at an intermediate path component.
func uniquePathValue(doc bson.Raw, path []string) bson.RawValue {
	value := doc.Lookup(path[0])
	for _, name := range path[1:] {
		if value.Type == bson.TypeArray {
			return value
		}
		child, ok := value.DocumentOK()
		if !ok {
			return bson.RawValue{}
		}
		value = child.Lookup(name)
	}
	return value
}
