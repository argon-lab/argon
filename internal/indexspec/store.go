// Package indexspec keeps each branch's secondary index definitions, so that
// a checkout can build the same indexes as the database the branch came from.
//
// Indexes are not versioned. A branch holds the latest definitions recorded
// for it: by an import (the source's indexes), by a follower (index changes
// on the source), or by capture (index changes made through a checkout of
// the branch). A branch without its own record uses its nearest ancestor's,
// as it is now, not as it was at the fork point.
package indexspec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// CollectionName is the metadata collection holding the definitions.
const CollectionName = "wal_index_specs"

// Store reads and writes index definitions in the metadata database.
type Store struct {
	coll *mongo.Collection
}

// NewStore returns a store on the metadata database db.
func NewStore(db *mongo.Database) *Store {
	return &Store{coll: db.Collection(CollectionName)}
}

type key struct {
	Branch     string `bson:"b"`
	Collection string `bson:"c"`
}

type record struct {
	ID        key        `bson:"_id"`
	Indexes   []bson.Raw `bson:"indexes"`
	UpdatedAt time.Time  `bson:"updated_at"`
}

// Lookup returns a branch by ID; BranchService.GetBranchByID satisfies it.
type Lookup func(branchID string) (*wal.Branch, error)

// Set replaces a branch's definitions for one collection.
func (s *Store) Set(ctx context.Context, branchID, collection string, specs []bson.Raw) error {
	clean := make([]bson.Raw, 0, len(specs))
	for _, spec := range specs {
		if c, ok := Clean(spec); ok {
			clean = append(clean, c)
		}
	}
	_, err := s.coll.ReplaceOne(ctx, bson.M{"_id": key{branchID, collection}},
		record{ID: key{branchID, collection}, Indexes: clean, UpdatedAt: time.Now()},
		options.Replace().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("record indexes of %s: %w", collection, err)
	}
	return nil
}

// Resolve returns the definitions that apply to a collection of branch: its
// own record, or else the nearest ancestor's. found is false when no branch
// in the chain has a record.
func (s *Store) Resolve(ctx context.Context, branch *wal.Branch, collection string, lookup Lookup) (specs []bson.Raw, found bool, err error) {
	for b, depth := branch, 0; b != nil; depth++ {
		if depth > 1000 {
			return nil, false, errors.New("branch ancestry is longer than 1000")
		}
		var r record
		err := s.coll.FindOne(ctx, bson.M{"_id": key{b.ID, collection}}).Decode(&r)
		if err == nil {
			return r.Indexes, true, nil
		}
		if !errors.Is(err, mongo.ErrNoDocuments) {
			return nil, false, fmt.Errorf("read indexes of %s: %w", collection, err)
		}
		if b.ParentID == "" {
			break
		}
		if b, err = lookup(b.ParentID); err != nil {
			return nil, false, err
		}
	}
	return nil, false, nil
}

// Created records indexes built on a collection of branch, replacing any
// definition with the same name. A branch without its own record starts from
// its ancestor's definitions.
func (s *Store) Created(ctx context.Context, branch *wal.Branch, collection string, specs []bson.Raw, lookup Lookup) error {
	return s.modify(ctx, branch, collection, lookup, func(current []bson.Raw) []bson.Raw {
		for _, spec := range specs {
			c, ok := Clean(spec)
			if !ok {
				continue
			}
			current = removeNamed(current, Name(c))
			current = append(current, c)
		}
		return current
	})
}

// Dropped removes the named indexes from a collection of branch.
func (s *Store) Dropped(ctx context.Context, branch *wal.Branch, collection string, names []string, lookup Lookup) error {
	return s.modify(ctx, branch, collection, lookup, func(current []bson.Raw) []bson.Raw {
		for _, name := range names {
			current = removeNamed(current, name)
		}
		return current
	})
}

// Forget removes every record of a deleted branch.
func (s *Store) Forget(ctx context.Context, branchID string) error {
	_, err := s.coll.DeleteMany(ctx, bson.M{"_id.b": branchID})
	return err
}

func (s *Store) modify(ctx context.Context, branch *wal.Branch, collection string, lookup Lookup, change func([]bson.Raw) []bson.Raw) error {
	current, _, err := s.Resolve(ctx, branch, collection, lookup)
	if err != nil {
		return err
	}
	next := change(append([]bson.Raw(nil), current...))
	return s.Set(ctx, branch.ID, collection, next)
}

func removeNamed(specs []bson.Raw, name string) []bson.Raw {
	out := specs[:0]
	for _, spec := range specs {
		if Name(spec) != name {
			out = append(out, spec)
		}
	}
	return out
}

// Name returns an index definition's name.
func Name(spec bson.Raw) string {
	name, _ := spec.Lookup("name").StringValueOK()
	return name
}

// Clean drops the fields listIndexes adds (v, ns) and refuses the _id index,
// which every collection has. ok is false for a definition that is not kept.
func Clean(spec bson.Raw) (bson.Raw, bool) {
	name := Name(spec)
	if name == "" || name == "_id_" {
		return nil, false
	}
	elems, err := spec.Elements()
	if err != nil {
		return nil, false
	}
	out := bson.D{}
	for _, e := range elems {
		switch e.Key() {
		case "v", "ns":
			continue
		}
		out = append(out, bson.E{Key: e.Key(), Value: e.Value()})
	}
	raw, err := bson.Marshal(out)
	if err != nil {
		return nil, false
	}
	return raw, true
}

// FromCollection lists a collection's secondary index definitions.
func FromCollection(ctx context.Context, coll *mongo.Collection) ([]bson.Raw, error) {
	cursor, err := coll.Indexes().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list indexes of %s: %w", coll.Name(), err)
	}
	var all []bson.Raw
	if err := cursor.All(ctx, &all); err != nil {
		return nil, fmt.Errorf("list indexes of %s: %w", coll.Name(), err)
	}
	specs := make([]bson.Raw, 0, len(all))
	for _, spec := range all {
		if c, ok := Clean(spec); ok {
			specs = append(specs, c)
		}
	}
	return specs, nil
}

// Build creates the definitions on coll, one index at a time. A TTL index is
// built without its expiry, so that a checkout never deletes its own
// documents. Notes describe each index that was changed or could not be
// built, for example a unique index the data violates; the others are built
// regardless.
func Build(ctx context.Context, coll *mongo.Collection, specs []bson.Raw) (built int, notes []string, err error) {
	for _, spec := range specs {
		elems, err := spec.Elements()
		if err != nil {
			return built, notes, err
		}
		def := bson.D{}
		ttl := false
		for _, e := range elems {
			if e.Key() == "expireAfterSeconds" {
				ttl = true
				continue
			}
			def = append(def, bson.E{Key: e.Key(), Value: e.Value()})
		}
		err = coll.Database().RunCommand(ctx, bson.D{
			{Key: "createIndexes", Value: coll.Name()},
			{Key: "indexes", Value: bson.A{def}},
		}).Err()
		if ctx.Err() != nil {
			return built, notes, ctx.Err()
		}
		if err != nil {
			notes = append(notes, fmt.Sprintf("index %s on %s was not built: %s", Name(spec), coll.Name(), firstLine(err.Error())))
			continue
		}
		built++
		if ttl {
			notes = append(notes, fmt.Sprintf("index %s on %s was built without its TTL expiry", Name(spec), coll.Name()))
		}
	}
	return built, notes, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
