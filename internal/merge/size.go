package merge

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	maxPlanBytes      = 16 * 1024 * 1024
	maxDisplayBytes   = 1024
	maxLastErrorBytes = 4096
)

// boundedAuditText limits bytes without splitting a UTF-8 character.
func boundedAuditText(text string, limit int) string {
	text = strings.ToValidUTF8(text, "�")
	if len(text) <= limit {
		return text
	}
	end := limit - len("…")
	for !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + "…"
}

// A retry clears the previous failure in its claim update. The largest
// possible additions are therefore either one error string or the complete
// applied and follower-settled record. Applied and pending have equal
// length. The two outcome timestamps more than cover the longer claim status.
func planAuditReserve() (int, error) {
	now := time.Unix(1, 0)
	applied, err := bson.Marshal(bson.M{
		"strategy": StrategyTheirs, "applied_at": now,
		"write_back": &WriteBackRecord{
			Session: primitive.Binary{Subtype: 4, Data: make([]byte, 16)},
			// MongoDB database names are at most 63 bytes.
			Database: strings.Repeat("d", 63), Display: strings.Repeat("d", maxDisplayBytes),
			Strategy: StrategyTheirs, StartedAt: now, CommittedAt: &now, SettledAt: &now,
		},
	})
	if err != nil {
		return 0, err
	}
	failed, err := bson.Marshal(bson.M{"write_back_last_error": strings.Repeat("e", maxLastErrorBytes)})
	if err != nil {
		return 0, err
	}
	// Only the elements are appended to an existing document.
	return max(len(applied), len(failed)) - 5, nil
}

func encodePendingPlan(plan *Plan) (bson.Raw, error) {
	raw, err := bson.Marshal(plan)
	if err != nil {
		return nil, err
	}
	reserve, err := planAuditReserve()
	if err != nil {
		return nil, err
	}
	if len(raw)+reserve > maxPlanBytes {
		return nil, fmt.Errorf("encoded merge plan is %d bytes and needs %d bytes for its outcome, exceeding MongoDB's 16 MiB limit (%d bytes). Before-images, after-images and conflicts share one document. Make the set of changes smaller for one reviewed plan", len(raw), reserve, maxPlanBytes)
	}
	return raw, nil
}
