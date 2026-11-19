package merge

import (
	"fmt"
	"strings"
	"testing"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/x/bsonx/bsoncore"
)

func TestWriteBack_PersistedMapImagesRequireVerifiedCapture(t *testing.T) {
	doc := bson.M{"_id": "box"}
	p := &Plan{
		FormatVersion: orderedPlanVersion, ProvenanceVersion: 1,
		Changes: []Change{{
			Collection: "items", DocumentID: "box", Before: doc, Document: doc,
		}},
		Conflicts: []Conflict{{
			Collection: "shipping", DocumentID: "box", Base: doc, Ours: doc, Theirs: doc,
		}},
	}
	raw, err := encodePendingPlan(p)
	require.NoError(t, err)
	var saved Plan
	require.NoError(t, bson.Unmarshal(raw, &saved))
	require.ElementsMatch(t, []string{"before", "document"}, saved.Changes[0].SortedImages)
	require.ElementsMatch(t, []string{"ours", "theirs", "base"}, saved.Conflicts[0].SortedImages)
	changes, _, err := resolveChanges(&saved, StrategyTheirs)
	require.NoError(t, err)
	err = prepareWriteBack(&saved, changes)
	require.ErrorContains(t, err, "unverified before-image")
	require.ErrorContains(t, err, "unverified after-image")
	require.ErrorContains(t, err, "items/box")
	require.ErrorContains(t, err, "shipping/box")
}

func TestWriteBack_EvidenceAfterConflictResolution(t *testing.T) {
	p := &Plan{FormatVersion: orderedPlanVersion, ProvenanceVersion: 1}
	for i := range 8 {
		id := fmt.Sprintf("box-%d", i)
		ours, theirs := bson.M{"_id": id}, bson.M{"_id": id, "status": "sent"}
		before, err := bson.Marshal(ours)
		require.NoError(t, err)
		after, err := wal.MarshalDocument(theirs)
		require.NoError(t, err)
		c := Conflict{
			Collection: "shipping", DocumentID: id, Ours: ours, Theirs: theirs,
			OursRaw: before, TheirsRaw: after, SortedImages: []string{"base", "ours", "theirs"},
		}
		if i == 7 {
			c.SortedImages = []string{"base"} // Neither strategy uses the base image.
		}
		p.Conflicts = append(p.Conflicts, c)
	}
	changes, _, err := resolveChanges(p, StrategyOurs)
	require.NoError(t, err)
	require.NoError(t, prepareWriteBack(p, changes))
	changes, _, err = resolveChanges(p, StrategyTheirs)
	require.NoError(t, err)
	err = prepareWriteBack(p, changes)
	require.ErrorContains(t, err, "unverified before-image")
	require.ErrorContains(t, err, "unverified after-image")
	for _, token := range []string{"(7 documents)", "shipping/box-0", "shipping/box-4", "and 2 more"} {
		require.Equal(t, 2, strings.Count(err.Error(), token), token)
	}
	for _, id := range []string{"box-5", "box-6", "box-7"} {
		require.NotContains(t, err.Error(), id)
	}
}

func TestWriteBack_RefusesRepeatedArrayKeys(t *testing.T) {
	array := bsoncore.BuildDocument(nil, bsoncore.AppendInt32Element(nil, "0", 1), bsoncore.AppendInt32Element(nil, "0", 2))
	id := bsoncore.AppendStringElement(nil, "_id", "box")
	raw := bsoncore.BuildDocument(nil, id, bsoncore.AppendArrayElement(nil, "contents", array))
	p := &Plan{FormatVersion: orderedPlanVersion, ProvenanceVersion: 1}
	for _, c := range []Change{
		{Collection: "items", DocumentID: "box", BeforeRaw: raw, Delete: true},
		{Collection: "items", DocumentID: "box", DocumentRaw: raw},
	} {
		err := prepareWriteBack(p, []Change{c})
		require.ErrorContains(t, err, "duplicate fields")
		require.ErrorContains(t, err, "items/box")
	}
}
