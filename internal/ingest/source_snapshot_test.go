package ingest

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// These documents have the shape returned by the snapshot aggregation. Shared
// subdocuments allow a case to change a single piece of stored evidence.
type storedSourceExample struct {
	document, branch, report, source, consistent bson.M
}

func newStoredSourceExample() storedSourceExample {
	at := primitive.Timestamp{T: 10, I: 1}
	consistent := bson.M{"lsn": int64(0), "cluster_time": at}
	source := bson.M{"database": "shop", "mode": "exact", "position": at, "consistent": consistent}
	report := bson.M{"state": "stopped", "head_lsn": int64(0), "source": source}
	branch := bson.M{"_id": "b", "name": "main", "follows": "shop", "head_lsn": int64(0)}
	document := bson.M{
		"branch": branch, "project": bson.M{"_id": "p"}, "capture_status": report,
		"resume_token": bson.M{"_data": "820000000a00000001"},
	}
	return storedSourceExample{document, branch, report, source, consistent}
}

func (e storedSourceExample) status(t *testing.T) (*Status, error) {
	t.Helper()
	if head, ok := e.branch["head_lsn"]; ok {
		e.document["branch_head"] = head
	}
	var snapshot captureSnapshot
	require.NoError(t, bson.Unmarshal(mustRaw(e.document), &snapshot))
	return snapshot.status()
}

func TestSourceSnapshotCheckpoint(t *testing.T) {
	cases := []struct {
		name   string
		change func(storedSourceExample)
		reason Reason
	}{
		{"explicit zero head", func(storedSourceExample) {}, ReasonExact},
		{"branch head missing at zero", func(e storedSourceExample) { delete(e.branch, "head_lsn") }, ReasonHeadMissing},
		{"branch head null at zero", func(e storedSourceExample) { e.branch["head_lsn"] = nil }, ReasonHeadMissing},
		{"saved head missing at zero", func(e storedSourceExample) { delete(e.report, "head_lsn") }, ReasonHeadMissing},
		{"branch moved", func(e storedSourceExample) { e.branch["head_lsn"] = int64(2) }, ReasonHeadChanged},
		{"verified head missing at zero", func(e storedSourceExample) { delete(e.consistent, "lsn") }, ReasonCheckpointUnverified},
		{"verified head null at zero", func(e storedSourceExample) { e.consistent["lsn"] = nil }, ReasonCheckpointUnverified},
		{"verified head differs", func(e storedSourceExample) { e.consistent["lsn"] = int64(1) }, ReasonCheckpointUnverified},
		{"verified time missing", func(e storedSourceExample) { delete(e.consistent, "cluster_time") }, ReasonCheckpointUnverified},
		{"verified time differs", func(e storedSourceExample) { e.consistent["cluster_time"] = primitive.Timestamp{T: 8} }, ReasonCheckpointUnverified},
		{"no consistent checkpoint", func(e storedSourceExample) { delete(e.source, "consistent") }, ReasonCheckpointUnverified},
		{"no source", func(e storedSourceExample) { delete(e.report, "source") }, ReasonPositionMissing},
		{"wrong database", func(e storedSourceExample) { e.source["database"] = "another" }, ReasonPositionMissing},
		{"position missing", func(e storedSourceExample) { delete(e.source, "position") }, ReasonPositionMissing},
		{"token missing", func(e storedSourceExample) { delete(e.document, "resume_token") }, ReasonPositionMissing},
		{"malformed token", func(e storedSourceExample) { e.document["resume_token"] = bson.M{"_data": "bad"} }, ReasonPositionMissing},
		{"older token", func(e storedSourceExample) { e.document["resume_token"] = bson.M{"_data": "820000000a00000000"} }, ReasonPositionMissing},
		{"later token without opening boundary", func(e storedSourceExample) {
			e.document["resume_token"] = bson.M{"_data": "820000000a00000002"}
		}, ReasonPositionMissing},
		{"later token differs from opening boundary", func(e storedSourceExample) {
			e.document["start_at"] = primitive.Timestamp{T: 9}
			e.document["resume_token"] = bson.M{"_data": "820000000a00000002"}
		}, ReasonPositionMissing},
		{"opening token skips multiple ticks", func(e storedSourceExample) {
			e.document["start_at"] = primitive.Timestamp{T: 10, I: 8}
			e.document["resume_token"] = bson.M{"_data": "820000000a00000008"}
		}, ReasonPositionMissing},
		{"idle opening boundary", func(e storedSourceExample) {
			e.document["start_at"] = primitive.Timestamp{T: 10, I: 2}
			e.document["resume_token"] = bson.M{"_data": "820000000a00000002"}
		}, ReasonExact},
		{"idle opening across ordinal rollover", func(e storedSourceExample) {
			at := primitive.Timestamp{T: 10, I: math.MaxUint32}
			e.source["position"], e.consistent["cluster_time"] = at, at
			e.document["start_at"] = primitive.Timestamp{T: 11}
			e.document["resume_token"] = bson.M{"_data": "820000000b00000000"}
		}, ReasonExact},
		{"absent mode", func(e storedSourceExample) { delete(e.source, "mode") }, ReasonModeUnknown},
		{"unknown mode", func(e storedSourceExample) { e.source["mode"] = "unexpected" }, ReasonModeUnknown},
		{"unknown state", func(e storedSourceExample) { e.report["state"] = "unexpected" }, ReasonStateUnknown},
		{"only open range LSN", func(e storedSourceExample) { e.source["inexact_from_lsn"] = int64(1) }, ReasonRangeInconsistent},
		{"only open range reason", func(e storedSourceExample) { e.source["inexact_reason"] = "repair" }, ReasonRangeInconsistent},
		{"missing project", func(e storedSourceExample) { delete(e.document, "project") }, ReasonProjectMissing},
		{"unpublished project", func(e storedSourceExample) { e.document["project"] = bson.M{"importing": true} }, ReasonImporting},
		{"checked out", func(e storedSourceExample) { e.branch["state"] = "live" }, ReasonCheckedOut},
		{"physical database", func(e storedSourceExample) { e.branch["physical_db"] = "argon_br_b" }, ReasonCheckedOut},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			e := newStoredSourceExample()
			test.change(e)
			s, err := e.status(t)
			require.NoError(t, err)
			require.Equal(t, test.reason, s.Health.Reason)
		})
	}
}

func TestSourceSnapshotMixedEvidence(t *testing.T) {
	e := newStoredSourceExample()
	e.branch["physical_db"] = "argon_br_b"
	e.report["state"] = "stopped"
	e.report["head_lsn"] = int64(9)
	e.report["error"] = "stream option rejected"
	e.report["owner"] = bson.M{"id": "a", "kind": "resync"}
	e.report["unverified"] = bson.M{"reason": "expired_owner"}
	e.source["mode"] = "catch_up"
	e.source["resync"] = bson.M{}
	e.source["inexact_reason"] = "resync"
	e.document["lease_fresh"] = false
	s, err := e.status(t)
	require.NoError(t, err)
	f := s.facts
	require.Equal(t, FollowerStopped, f.State)
	require.Equal(t, CheckpointModeCatchUp, f.Mode)
	require.Equal(t, ProjectPublished, f.Project)
	require.True(t, f.CheckedOut)
	require.Equal(t, HeadDifferent, f.Head)
	require.True(t, f.PositionPresent)
	require.True(t, f.HasError)
	require.Equal(t, "stream option rejected", f.StoredError)
	require.Equal(t, OwnerExpired, f.Owner)
	require.True(t, f.Unverified)
	require.True(t, f.UnfinishedResync)
	require.True(t, f.OpenRange)
	require.False(t, f.ExactPointAtHead)
	require.Equal(t, ReasonCheckedOut, s.Health.Reason)
	require.Contains(t, s.Health.Message, f.StoredError)

	e.document["lease_fresh"] = true
	s, err = e.status(t)
	require.NoError(t, err)
	require.Equal(t, OwnerLive, s.facts.Owner)
	delete(e.report, "owner")
	s, err = e.status(t)
	require.NoError(t, err)
	require.Equal(t, OwnerNone, s.facts.Owner)

	e = newStoredSourceExample()
	e.report["source"] = "malformed document"
	s, err = e.status(t)
	require.Error(t, err)
	require.Nil(t, s)
}
