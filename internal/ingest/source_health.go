package ingest

import (
	"errors"
	"fmt"
	"strings"
)

type FollowerState uint8

const (
	FollowerAbsent FollowerState = iota
	FollowerUnknown
	FollowerStarting
	FollowerRunning
	FollowerCatchingUp
	FollowerRetrying
	FollowerResyncing
	FollowerStopped
	FollowerDegraded
)

func followerState(stored string) FollowerState {
	switch stored {
	case "":
		return FollowerAbsent
	case "starting":
		return FollowerStarting
	case "running":
		return FollowerRunning
	case "catching_up":
		return FollowerCatchingUp
	case "retrying":
		return FollowerRetrying
	case "resyncing":
		return FollowerResyncing
	case "stopped":
		return FollowerStopped
	case "degraded":
		return FollowerDegraded
	default:
		return FollowerUnknown
	}
}

func (s FollowerState) expectsOwner() bool {
	switch s {
	case FollowerStarting, FollowerRunning, FollowerCatchingUp, FollowerRetrying, FollowerResyncing:
		return true
	}
	return false
}

type CheckpointMode uint8

const (
	CheckpointModeAbsent CheckpointMode = iota
	CheckpointModeUnknown
	CheckpointModeExact
	CheckpointModeCatchUp
)

func checkpointMode(stored string) CheckpointMode {
	switch stored {
	case "":
		return CheckpointModeAbsent
	case ModeExact:
		return CheckpointModeExact
	case ModeCatchUp:
		return CheckpointModeCatchUp
	default:
		return CheckpointModeUnknown
	}
}

type OwnerState uint8

const (
	OwnerNone OwnerState = iota
	OwnerLive
	OwnerExpired
)

type HeadState uint8

const (
	HeadMissing HeadState = iota
	HeadEqual
	HeadDifferent
)

type ProjectState uint8

const (
	ProjectMissing ProjectState = iota
	ProjectImporting
	ProjectPublished
)

type PositionDigest uint8

const (
	DigestAbsent PositionDigest = iota
	DigestMatches
	DigestDiffers
)

// Facts describe one metadata snapshot. An absent or unfamiliar report
// cannot be mistaken for an exact checkpoint.
type Facts struct {
	Project          ProjectState
	CheckedOut       bool
	State            FollowerState
	CleanStart       bool
	Mode             CheckpointMode
	HasError         bool
	Owner            OwnerState
	Unverified       bool
	UnfinishedResync bool
	Head             HeadState
	PositionPresent  bool
	PositionDigest   PositionDigest
	// ExactPointAtHead compares the stored exact point with the head and
	// source position. A takeover must independently clear Unverified.
	ExactPointAtHead bool
	OpenRange        bool

	// These details describe the evidence to the operator. They have no
	// effect on the reason, command admission or checkpoint verification.
	BranchName   string
	StoredError  string
	OwnerLoss    string
	HeadMismatch string
}

type VerdictKind string

const (
	SourceHealthy   VerdictKind = "healthy"
	SourceUnhealthy VerdictKind = "unhealthy"
	SourceImporting VerdictKind = "importing"
)

type Reason string

const (
	ReasonImporting            Reason = "importing"
	ReasonProjectMissing       Reason = "project_missing"
	ReasonCheckedOut           Reason = "checked_out"
	ReasonHeadChanged          Reason = "head_changed"
	ReasonResyncUnfinished     Reason = "resync_unfinished"
	ReasonPositionMissing      Reason = "position_missing"
	ReasonHeadMissing          Reason = "head_missing"
	ReasonModeUnknown          Reason = "mode_unknown"
	ReasonRangeInconsistent    Reason = "range_inconsistent"
	ReasonOwnerExpired         Reason = "owner_expired"
	ReasonDegraded             Reason = "degraded"
	ReasonUnverified           Reason = "unverified"
	ReasonOwnerMissing         Reason = "owner_missing"
	ReasonRetrying             Reason = "retrying"
	ReasonCaptureError         Reason = "capture_error"
	ReasonCatchUp              Reason = "catch_up"
	ReasonCheckpointUnverified Reason = "checkpoint_unverified"
	ReasonStarting             Reason = "starting"
	ReasonRestarting           Reason = "restarting"
	ReasonResyncing            Reason = "resyncing"
	ReasonStateUnknown         Reason = "state_unknown"
	ReasonExact                Reason = "exact"
)

type Verdict struct {
	Kind         VerdictKind `json:"kind"`
	Reason       Reason      `json:"reason"`
	Message      string      `json:"message"`
	NextAction   string      `json:"next_action"`
	LeaseExpired bool        `json:"-"`
}

const (
	startFollow = "Start `argon source follow` to catch up and verify a checkpoint."
	runResync   = "Run `argon source resync`, then start `argon source follow`."
)

// Evaluate gives priority to publication and checkout, then to repairs that
// ordinary replay cannot perform, ownership loss, capture failures, and open
// ranges. Only an exact verified head can reach the healthy cases below.
func Evaluate(f Facts) Verdict {
	missingOwner := f.Owner == OwnerNone && f.State.expectsOwner()
	v := Verdict{Kind: SourceUnhealthy, LeaseExpired: f.Owner == OwnerExpired || missingOwner}
	resume := startFollow
	if f.Owner == OwnerLive {
		resume = "Wait for the follower to reach an exact checkpoint."
	}

	switch {
	case f.Project == ProjectImporting:
		v.Kind, v.Reason = SourceImporting, ReasonImporting
		v.Message, v.NextAction = "The online import is still in progress.", "Wait for the import to publish the project."
	case f.Project != ProjectPublished:
		v.Reason = ReasonProjectMissing
		v.Message, v.NextAction = "The branch has no project record.", "Restore the missing project metadata."
	case f.CheckedOut:
		v.Reason = ReasonCheckedOut
		v.Message = fmt.Sprintf("Followed branch %s was checked out by another process.", f.BranchName)
		v.NextAction = "Upgrade every process using this metadata, stop writers to the physical database, run `argon release`, then run `argon source resync`."
	case f.Head == HeadDifferent:
		v.Reason = ReasonHeadChanged
		v.Message = "The branch head moved without the follower."
		if f.HeadMismatch != "" {
			v.Message = f.HeadMismatch
		}
		v.NextAction = "Upgrade every process using this metadata, then run `argon source resync`."
	case f.UnfinishedResync:
		v.Reason = ReasonResyncUnfinished
		v.Message = "A resync of the followed branch has not finished."
		v.NextAction = "Let the resync finish. If interrupted, run `argon source resync` again."
	case !f.PositionPresent || f.PositionDigest == DigestDiffers:
		v.Reason = ReasonPositionMissing
		v.Message = fmt.Sprintf("Following cannot resume because the saved source position of branch %s is missing or was replaced by another process.", f.BranchName)
		v.NextAction = "Upgrade every process using this metadata, then run `argon source resync`."
	case f.Head != HeadEqual:
		v.Reason = ReasonHeadMissing
		v.Message, v.NextAction = "The branch or its checkpoint has no recorded head LSN.", runResync
	case f.Mode != CheckpointModeExact && f.Mode != CheckpointModeCatchUp:
		v.Reason = ReasonModeUnknown
		v.Message, v.NextAction = "The checkpoint has no recognized source mode.", runResync
	case f.OpenRange && f.Mode == CheckpointModeExact:
		v.Reason = ReasonRangeInconsistent
		v.Message, v.NextAction = "The checkpoint records both exact mode and an open inexact range.", runResync
	case f.Mode == CheckpointModeExact && !f.ExactPointAtHead:
		v.Reason = ReasonCheckpointUnverified
		v.Message, v.NextAction = "The recorded exact point does not establish the branch head at the saved source position.", runResync
	case f.State == FollowerAbsent || f.State == FollowerUnknown:
		v.Reason = ReasonStateUnknown
		v.Message, v.NextAction = "The follower has no recognized state for this checkpoint.", runResync
	case f.Owner == OwnerExpired:
		v.Reason = ReasonOwnerExpired
		v.Message, v.NextAction = "The source owner's lease expired.", startFollow
	case f.State == FollowerDegraded:
		v.Reason = ReasonDegraded
		v.Message, v.NextAction = "The last follower reported a capture failure.", runResync
	case f.Unverified:
		v.Reason = ReasonUnverified
		v.Message, v.NextAction = "The checkpoint is unverified after the previous run lost its lease.", startFollow
		if f.Owner == OwnerLive {
			v.NextAction = "The replacement is catching up. Wait for it to verify a checkpoint."
		}
	case missingOwner:
		v.Reason = ReasonOwnerMissing
		v.Message, v.NextAction = "The last live source report was left without a lease owner.", startFollow
	case f.State == FollowerRetrying:
		v.Reason = ReasonRetrying
		v.Message = "Following is behind the source and currently unable to read it."
		v.NextAction = "Restore access to the source and wait for the stream to reopen."
	case f.HasError:
		v.Reason = ReasonCaptureError
		v.Message, v.NextAction = "The last capture reported an error.", "Correct the reported error. "+resume
	case f.Mode == CheckpointModeCatchUp || f.State == FollowerCatchingUp:
		v.Reason = ReasonCatchUp
		v.Message, v.NextAction = "The branch is catching up and has no exact checkpoint for its current head.", resume
	case f.State == FollowerStarting && f.CleanStart && f.Owner == OwnerLive:
		v.Kind, v.Reason = SourceHealthy, ReasonRestarting
		v.Message, v.NextAction = "A live owner is reopening the stream from a verified exact checkpoint.", "Wait for the follower to be ready before write-back."
	case f.State == FollowerStarting:
		v.Reason = ReasonStarting
		v.Message, v.NextAction = "The follower is opening its source stream.", resume
	case f.State == FollowerResyncing:
		v.Reason = ReasonResyncing
		v.Message, v.NextAction = "The last report records a resync in progress.", "Wait for the resync to finish."
	case (f.State == FollowerRunning && f.Owner == OwnerLive) ||
		(f.State == FollowerStopped && (f.Owner == OwnerNone || f.Owner == OwnerLive)):
		v.Kind, v.Reason = SourceHealthy, ReasonExact
		v.Message, v.NextAction = "The branch head is a verified exact source checkpoint.", "No repair is needed."
	default:
		v.Reason = ReasonStateUnknown
		v.Message, v.NextAction = "The follower has no recognized state for this checkpoint.", runResync
	}

	if f.Unverified || v.LeaseExpired {
		v.Message = appendEvidence(v.Message, f.OwnerLoss)
	}
	if f.HasError {
		v.Message = appendEvidence(v.Message, f.StoredError)
	}
	return v
}

func appendEvidence(message, detail string) string {
	if detail == "" || strings.Contains(message, detail) {
		return message
	}
	return strings.TrimRight(message, ".") + ". " + detail
}

func (v Verdict) HealthError() error {
	if v.Kind == SourceHealthy {
		return nil
	}
	return errors.New(strings.TrimSpace(v.Message + " " + v.NextAction))
}

// DisplayState keeps the last report visible unless the snapshot itself
// establishes a loss of ownership or a structural problem with the position.
func (v Verdict) DisplayState(stored string) string {
	switch v.Reason {
	case ReasonCheckedOut, ReasonPositionMissing, ReasonHeadMissing, ReasonHeadChanged, ReasonModeUnknown, ReasonRangeInconsistent, ReasonDegraded:
		return "degraded"
	}
	if v.LeaseExpired {
		return "stopped"
	}
	return stored
}

// FollowAdmission validates a start or stream retry. A stored stream failure can
// be retried from a valid position. A checkout or replaced position cannot.
func (v Verdict) FollowAdmission() error {
	switch v.Reason {
	case ReasonExact, ReasonOwnerExpired, ReasonOwnerMissing, ReasonUnverified, ReasonDegraded,
		ReasonRetrying, ReasonCaptureError, ReasonCatchUp, ReasonStarting, ReasonRestarting, ReasonResyncing:
		return nil
	case ReasonResyncUnfinished:
		return unfinishedResyncError{&ConfigurationError{v.HealthError().Error()}}
	case ReasonImporting, ReasonProjectMissing:
		return &ConfigurationError{v.HealthError().Error()}
	default:
		return &CaptureError{Message: v.HealthError().Error()}
	}
}

// ResyncAdmission reports whether the saved position needs a read-only resume
// probe before the comparison is admitted. Forced repair still requires a
// published project and a released branch.
func (v Verdict) ResyncAdmission(force bool) (probeResume bool, err error) {
	switch v.Reason {
	case ReasonImporting, ReasonProjectMissing, ReasonCheckedOut:
		return false, v.FollowAdmission()
	case ReasonOwnerExpired, ReasonOwnerMissing, ReasonResyncUnfinished, ReasonDegraded,
		ReasonPositionMissing, ReasonHeadChanged, ReasonHeadMissing, ReasonModeUnknown, ReasonRangeInconsistent, ReasonCheckpointUnverified, ReasonStateUnknown:
		return false, nil
	case ReasonUnverified:
		return !force, nil
	default:
		if force {
			return false, nil
		}
		return false, &ConfigurationError{"following has not ended as degraded, so there is nothing a resync has to repair. `argon source follow` continues from the saved position. A resync compares the whole source with the branch and records the range it writes as inexact history. Pass --force to run one anyway"}
	}
}

func (v Verdict) WriteBackAdmission() error {
	switch v.Reason {
	case ReasonExact:
		return nil
	case ReasonRestarting:
		return errors.New("write-back refused: wait for the follower to be ready")
	}
	prefix := "write-back refused"
	switch v.Reason {
	case ReasonDegraded:
		prefix += ": following ended as degraded"
	case ReasonCheckedOut, ReasonPositionMissing, ReasonHeadMissing, ReasonHeadChanged, ReasonModeUnknown, ReasonRangeInconsistent:
		prefix += ": following is degraded"
	}
	return fmt.Errorf("%s: %w", prefix, v.HealthError())
}
