package merge

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"

	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
)

// All variable-size fields carry an unsigned 64-bit big-endian byte length.
// Lists start with an element count. Optional fields start with a presence byte.
type approvalEncoder struct{ hash.Hash }

func (e approvalEncoder) number(n uint64) {
	var field [8]byte
	binary.BigEndian.PutUint64(field[:], n)
	_, _ = e.Write(field[:])
}

func (e approvalEncoder) field(b []byte) {
	e.number(uint64(len(b)))
	_, _ = e.Write(b)
}

func (e approvalEncoder) optional(b []byte) {
	if b == nil {
		_, _ = e.Write([]byte{0})
		return
	}
	_, _ = e.Write([]byte{1})
	e.field(b)
}

func (e approvalEncoder) strings(values []string) {
	e.number(uint64(len(values)))
	for _, value := range values {
		e.field([]byte(value))
	}
}

func (p *Plan) approvalEligible() bool {
	return p.FormatVersion == orderedPlanVersion && p.ProvenanceVersion == provenanceVersion
}

// A lossless proposal names the same typed key that apply will pass to MongoDB.
// Some BSON values decode identically despite having different bytes.
func (c *Change) approvalKey() ([]byte, error) {
	if c.ID == nil && c.storedKey.Type == 0 {
		return nil, nil
	}
	if c.ID != nil && wal.DocumentIDString(c.ID) == c.DocumentID {
		raw, err := wal.MarshalDocument(bson.D{{Key: "key", Value: c.ID}})
		if err != nil {
			return nil, err
		}
		typed := raw.Lookup("key")
		if c.storedKey.Type == 0 || (typed.Type == c.storedKey.Type && bytes.Equal(typed.Value, c.storedKey.Value)) {
			return append([]byte{byte(typed.Type)}, typed.Value...), nil
		}
	}
	return nil, errors.New("inconsistent document_key in merge plan; preview again with this version using --output json")
}

func (e approvalEncoder) images(images ...planImage) error {
	for _, image := range images {
		if image.raw == nil && image.doc != nil {
			return errors.New("merge plan contains an unencoded image; save the plan before obtaining approval")
		}
		e.optional(image.raw)
	}
	return nil
}

// ApprovalDigest binds a source write to one version of a saved proposal.
// The document_id is the type-preserving WAL key used by documentIDValue.
func ApprovalDigest(p *Plan) (string, error) {
	if !p.approvalEligible() {
		return "", errors.New(oldPlanEvidence)
	}
	e := approvalEncoder{sha256.New()}
	e.field([]byte("argon.merge.write-back"))
	e.number(1) // Digest scheme version.
	e.number(uint64(p.FormatVersion))
	e.number(uint64(p.ProvenanceVersion))
	e.field(p.ID[:])
	e.field([]byte(p.ProjectID))
	e.field([]byte(p.TargetBranchID))
	e.field([]byte(p.TargetBranch))
	e.field([]byte(p.SourceBranchID))
	e.field([]byte(p.SourceBranch))
	e.number(uint64(p.SourceHead))
	e.number(uint64(p.TargetHead))
	e.number(uint64(p.BaseLSN))
	e.number(uint64(p.SourceVersion))
	e.number(uint64(p.TargetVersion))
	e.field([]byte(p.FollowedDatabase))
	e.field([]byte(p.FollowedReplicaSetID))
	e.number(uint64(len(p.Changes)))
	for _, c := range p.Changes {
		e.field([]byte(c.Collection))
		e.field([]byte(c.DocumentID))
		// document_key, when present, contributes the BSON type and value.
		key, err := c.approvalKey()
		if err != nil {
			return "", err
		}
		e.optional(key)
		switch {
		case c.Delete:
			e.field([]byte("delete"))
		case c.BeforeRaw == nil:
			e.field([]byte("insert"))
		default:
			e.field([]byte("replace"))
		}
		if err := e.images(planImage{"before", c.Before, c.BeforeRaw}, planImage{"document", c.Document, c.DocumentRaw}); err != nil {
			return "", err
		}
		e.strings(c.SortedImages)
	}
	e.number(uint64(len(p.Conflicts)))
	for _, c := range p.Conflicts {
		e.field([]byte(c.Collection))
		e.field([]byte(c.DocumentID))
		if err := e.images(c.cliImages()...); err != nil {
			return "", err
		}
		e.strings(c.SortedImages)
	}
	return hex.EncodeToString(e.Sum(nil)), nil
}

const approvalHelp = "run argon merge list -p <project> --output json and pass that plan's approval_digest with --approve. For an older plan, preview again with this version"

// ValidateApprovalDigest checks the required lowercase SHA-256 spelling.
func ValidateApprovalDigest(approved string) error {
	if len(approved) == sha256.Size*2 {
		for _, ch := range []byte(approved) {
			if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
				return errors.New("invalid --approve digest; " + approvalHelp)
			}
		}
		return nil
	}
	return errors.New("--write-back requires --approve with a 64-character lowercase hex digest; " + approvalHelp)
}

// CheckApproval verifies the digest against this record without disclosing it.
func (p *Plan) CheckApproval(approved string) error {
	expected, err := ApprovalDigest(p)
	if err != nil {
		return err
	}
	if err := ValidateApprovalDigest(approved); err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(expected), []byte(approved)) != 1 {
		return errors.New("approval digest does not match the saved plan; " + approvalHelp)
	}
	return nil
}
