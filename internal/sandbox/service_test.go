package sandbox

import (
	"context"
	"testing"
	"time"
)

func TestExtendRejectsNonPositiveTTL(t *testing.T) {
	service := &Service{}
	for _, ttl := range []time.Duration{0, -time.Minute} {
		if _, err := service.Extend(context.Background(), "branch", ttl); err == nil {
			t.Errorf("Extend(%s) succeeded; want an error", ttl)
		}
	}
}
