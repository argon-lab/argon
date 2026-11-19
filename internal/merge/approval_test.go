package merge

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/wal"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func approvalPlan(t *testing.T) *Plan {
	t.Helper()
	image := func(n int32) bson.Raw {
		t.Helper()
		raw, err := bson.Marshal(bson.D{{Key: "v", Value: n}})
		require.NoError(t, err)
		return raw
	}
	return &Plan{
		FormatVersion: 3, ProvenanceVersion: 1,
		ID:        primitive.ObjectID{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11},
		ProjectID: "project", TargetBranchID: "target", TargetBranch: "main",
		SourceBranchID: "source", SourceBranch: "edits", SourceHead: 7, TargetHead: 5,
		BaseLSN: 2, SourceVersion: 3, TargetVersion: 4,
		FollowedDatabase: "inventory", FollowedReplicaSetID: "rs-id",
		Changes: []Change{
			{Collection: "items", DocumentID: "x", ID: "x", BeforeRaw: image(1), DocumentRaw: image(2), SortedImages: []string{"before", "document"}},
			{Collection: "items", DocumentID: "y", DocumentRaw: image(5)},
		},
		Conflicts: []Conflict{
			{Collection: "items", DocumentID: "z", BaseRaw: image(1), OursRaw: image(2), TheirsRaw: image(3), SortedImages: []string{"ours", "theirs"}},
			{Collection: "items", DocumentID: "w"},
		},
	}
}

func TestWriteBack_ApprovalDigestV1(t *testing.T) {
	p := approvalPlan(t)
	want, err := ApprovalDigest(p)
	require.NoError(t, err)
	require.Equal(t, "b6d7d74082a2345e3289d2331f494fb0bbee6e3f73c6a3e8bf17a3d8853b8233", want)
	data, err := bson.Marshal(p)
	require.NoError(t, err)
	var loaded Plan
	require.NoError(t, bson.Unmarshal(data, &loaded))
	require.NoError(t, loaded.CheckApproval(want))

	for name, change := range map[string]func(*Plan){
		"id":                  func(p *Plan) { p.ID[0] ^= 1 },
		"project":             func(p *Plan) { p.ProjectID += "2" },
		"target ID":           func(p *Plan) { p.TargetBranchID += "2" },
		"target name":         func(p *Plan) { p.TargetBranch += "2" },
		"source ID":           func(p *Plan) { p.SourceBranchID += "2" },
		"source name":         func(p *Plan) { p.SourceBranch += "2" },
		"source head":         func(p *Plan) { p.SourceHead ^= 1 },
		"target head":         func(p *Plan) { p.TargetHead ^= 1 },
		"fork point":          func(p *Plan) { p.BaseLSN ^= 1 },
		"source version":      func(p *Plan) { p.SourceVersion ^= 1 },
		"target version":      func(p *Plan) { p.TargetVersion ^= 1 },
		"database":            func(p *Plan) { p.FollowedDatabase += "2" },
		"deployment":          func(p *Plan) { p.FollowedReplicaSetID += "2" },
		"change collection":   func(p *Plan) { p.Changes[0].Collection += "2" },
		"change key":          func(p *Plan) { p.Changes[1].DocumentID = wal.DocumentIDString(int64(0)) },
		"change typed key":    func(p *Plan) { p.Changes[0].DocumentID, p.Changes[0].ID = wal.DocumentIDString(int32(1)), int32(1) },
		"change key presence": func(p *Plan) { p.Changes[1].ID = "y" },
		"operation":           func(p *Plan) { p.Changes[0].Delete = true },
		"before":              func(p *Plan) { p.Changes[0].BeforeRaw[7] ^= 1 },
		"after":               func(p *Plan) { p.Changes[0].DocumentRaw[7] ^= 1 },
		"before presence":     func(p *Plan) { p.Changes[1].BeforeRaw = bson.Raw{} },
		"after absence":       func(p *Plan) { p.Changes[1].DocumentRaw = nil },
		"change marks":        func(p *Plan) { p.Changes[0].SortedImages[0] += "2" },
		"change mark order":   func(p *Plan) { slices.Reverse(p.Changes[0].SortedImages) },
		"change mark count":   func(p *Plan) { p.Changes[0].SortedImages = p.Changes[0].SortedImages[:1] },
		"change order":        func(p *Plan) { slices.Reverse(p.Changes) },
		"change removed":      func(p *Plan) { p.Changes = p.Changes[:1] },
		"change added":        func(p *Plan) { p.Changes = append(p.Changes, Change{}) },
		"conflict collection": func(p *Plan) { p.Conflicts[0].Collection += "2" },
		"conflict key":        func(p *Plan) { p.Conflicts[0].DocumentID = wal.DocumentIDString(int32(1)) },
		"base":                func(p *Plan) { p.Conflicts[0].BaseRaw[7] ^= 1 },
		"ours":                func(p *Plan) { p.Conflicts[0].OursRaw[7] ^= 1 },
		"theirs":              func(p *Plan) { p.Conflicts[0].TheirsRaw[7] ^= 1 },
		"base presence":       func(p *Plan) { p.Conflicts[1].BaseRaw = bson.Raw{} },
		"ours presence":       func(p *Plan) { p.Conflicts[1].OursRaw = bson.Raw{} },
		"theirs presence":     func(p *Plan) { p.Conflicts[1].TheirsRaw = bson.Raw{} },
		"conflict marks":      func(p *Plan) { p.Conflicts[0].SortedImages[0] += "2" },
		"conflict mark order": func(p *Plan) { slices.Reverse(p.Conflicts[0].SortedImages) },
		"conflict mark count": func(p *Plan) { p.Conflicts[0].SortedImages = p.Conflicts[0].SortedImages[:1] },
		"conflict order":      func(p *Plan) { slices.Reverse(p.Conflicts) },
		"conflict removed":    func(p *Plan) { p.Conflicts = p.Conflicts[:1] },
		"conflict added":      func(p *Plan) { p.Conflicts = append(p.Conflicts, Conflict{}) },
	} {
		t.Run(name, func(t *testing.T) {
			p := approvalPlan(t)
			change(p)
			digest, err := ApprovalDigest(p)
			require.NoError(t, err)
			require.NotEqual(t, want, digest)
			require.ErrorContains(t, p.CheckApproval(want), "does not match")
		})
	}
	t.Run("typed numeric keys", func(t *testing.T) {
		p := approvalPlan(t)
		p.Conflicts[0].DocumentID = wal.DocumentIDString(int32(7))
		intDigest, err := ApprovalDigest(p)
		require.NoError(t, err)
		p.Conflicts[0].DocumentID = wal.DocumentIDString(int64(7))
		require.ErrorContains(t, p.CheckApproval(intDigest), "does not match")
	})
	t.Run("empty and absent after", func(t *testing.T) {
		p := approvalPlan(t)
		p.Changes[1].DocumentRaw = nil
		absent, err := ApprovalDigest(p)
		require.NoError(t, err)
		p.Changes[1].DocumentRaw = bson.Raw{}
		require.ErrorContains(t, p.CheckApproval(absent), "does not match")
	})
	for _, versions := range [][2]int{{2, 0}, {3, 0}, {3, 2}, {4, 1}} {
		p := approvalPlan(t)
		p.FormatVersion, p.ProvenanceVersion = versions[0], versions[1]
		digest, err := ApprovalDigest(p)
		require.Empty(t, digest)
		require.ErrorContains(t, err, "preview again with this version")
	}
}

func TestWriteBack_ApprovalDigestExcludesOutcome(t *testing.T) {
	now := time.Now()
	for name, change := range map[string]func(*Plan){
		"status":       func(p *Plan) { p.Status = StatusApplied },
		"strategy":     func(p *Plan) { p.Strategy = StrategyTheirs },
		"created time": func(p *Plan) { p.CreatedAt = now },
		"applied time": func(p *Plan) { p.AppliedAt = &now },
		"last error":   func(p *Plan) { p.WriteBackLastError = "source unavailable" },
		"claim": func(p *Plan) {
			p.WriteBack = &WriteBackRecord{
				ApprovedDigest: strings.Repeat("a", 64), Database: "target", Display: "host",
				Session:  primitive.Binary{Subtype: 4, Data: []byte("session")},
				Strategy: StrategyTheirs, Changes: 2, StartedAt: now, CommittedAt: &now, SettledAt: &now,
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := approvalPlan(t)
			want, err := ApprovalDigest(p)
			require.NoError(t, err)
			change(p)
			require.NoError(t, p.CheckApproval(want))
		})
	}
}

func TestWriteBack_ApprovalRejectsHiddenDocumentKey(t *testing.T) {
	p := approvalPlan(t)
	p.FollowedDatabase, p.Status = "inventory", StatusPending
	id := bson.D{{Key: "region", Value: primitive.Regex{Pattern: "x", Options: "im"}}}
	p.Changes[0].ID, p.Changes[0].DocumentID = id, wal.DocumentIDString(id)
	change, err := bson.Marshal(p.Changes[0])
	require.NoError(t, err)
	// The driver preserves both option strings when reading and sorts them
	// when encoding the typed key. Only one matches the stored BSON bytes.
	regex := bson.Raw(change).Lookup("document_key", "region").Value
	copy(regex, []byte("x\x00mi\x00"))
	require.NoError(t, bson.Unmarshal(change, &p.Changes[0]))
	require.Equal(t, p.Changes[0].DocumentID, wal.DocumentIDString(p.Changes[0].ID))
	digest, err := ApprovalDigest(p)
	require.Empty(t, digest)
	require.ErrorContains(t, err, "document_key")
	body, err := p.CLIJSON(true)
	require.Empty(t, body)
	require.ErrorContains(t, err, "document_key")

	empty, err := bson.Marshal(bson.D{})
	require.NoError(t, err)
	require.NoError(t, bson.Unmarshal(empty, &p.Changes[0]))
	require.Nil(t, p.Changes[0].Before)
	require.Nil(t, p.Changes[0].Document)
	require.Nil(t, p.Changes[0].ID)
	require.NoError(t, bson.Unmarshal(empty, &p.Conflicts[0]))
	require.Nil(t, p.Conflicts[0].Base)
	require.Nil(t, p.Conflicts[0].Ours)
	require.Nil(t, p.Conflicts[0].Theirs)
}
