package wal_test

import (
	"strings"
	"testing"

	"github.com/argon-lab/argon/v2/internal/source"
)

// Redact must remove secrets and leave the rest of the message readable.
func TestSource_RedactLeavesMessagesReadable(t *testing.T) {
	t.Run("password that is a substring of the mask", func(t *testing.T) {
		uri := "mongodb://app:a@cluster0.example.net/shop"
		in := "the source did not answer: server selection error: dial tcp cluster0.example.net:27017: i/o timeout"
		out := source.Redact(in, uri)
		t.Logf("in : %s", in)
		t.Logf("out: %s", out)
		if strings.Contains(out, "[red[red") {
			t.Errorf("mask was re-masked into nested noise: %q", out)
		}
		if !strings.Contains(out, "i/o timeout") {
			t.Errorf("the cause of the failure is no longer readable: %q", out)
		}
	})
	t.Run("password equal to a common word", func(t *testing.T) {
		uri := "mongodb://app:mongo@mongo:27017/shop"
		in := "dial tcp: lookup mongo: no such host; mongo-driver"
		out := source.Redact(in, uri)
		t.Logf("out: %s", out)
		// The password cannot be told from the host name of the same
		// spelling, and a password is not printed: both go, once.
		if strings.Contains(out, "mongo") || strings.Contains(out, "[red[red") {
			t.Errorf("the password must be removed, without nesting: %q", out)
		}
	})
	t.Run("non-secret option value", func(t *testing.T) {
		uri := "mongodb://app:S3cretPassw0rd@db.example.net/shop?replicaSet=rs-prod-01&readPreference=secondaryPreferred"
		in := `the connection string leads to replica set "rs-prod-01" with readPreference secondaryPreferred, not the one this project follows`
		out := source.Redact(in, uri)
		t.Logf("out: %s", out)
		if !strings.Contains(out, "rs-prod-01") {
			t.Errorf("replica set name masked: %q", out)
		}
		if !strings.Contains(out, "secondaryPreferred") {
			t.Errorf("read preference masked: %q", out)
		}
	})
}
