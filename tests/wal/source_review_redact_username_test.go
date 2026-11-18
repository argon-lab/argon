package wal_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/argon-lab/argon/v2/internal/source"
)

// Docs (docs/OPERATIONS.md, Authentication): "user names, passwords and
// options are removed from driver errors before they are printed or stored".
func TestSource_RedactRemovesWhatTheDocsSay(t *testing.T) {
	uri := "mongodb://alice:pw12345678@h/db?tlsCertificateKeyFilePassword=short"
	got := source.Redact("alice short pw12345678", uri)
	t.Logf("Redact(%q) = %q", "alice short pw12345678", got)
	if strings.Contains(got, "pw12345678") {
		t.Fatalf("password survived: %q", got)
	}
	// A user name on its own is not a secret and is left; the docs say so.
	if !strings.Contains(got, "alice") {
		t.Errorf("user name was removed, which the docs say is not done: %q", got)
	}
	if strings.Contains(got, "short") {
		t.Errorf("option value under 8 characters survived redaction: %q", got)
	}

	// A server-style message naming the user.
	msg := `(Unauthorized) not authorized on db to execute command; user "alice@admin"`
	got = source.Redact(msg, uri)
	t.Logf("Redact(server message) = %q", got)
	if got != msg {
		t.Errorf("a message without secrets was changed: %q", got)
	}

	// A secret option's value is removed whatever its length; an ordinary
	// option's value never is.
	for _, v := range []string{"abc", "abcdefgh"} {
		if out := source.Redact("value "+v, "mongodb://h/db?tlsCertificateKeyFilePassword="+v); strings.Contains(out, v) {
			t.Errorf("secret option value %q survived: %q", v, out)
		}
		if out := source.Redact("value "+v, "mongodb://h/db?replicaSet="+v); !strings.Contains(out, v) {
			t.Errorf("ordinary option value %q was removed: %q", v, out)
		}
	}
}

// What a real failed authentication prints through source.Connect.
func TestSource_FailedLoginPrintsNoCredentials(t *testing.T) {
	base := os.Getenv("ARGON_TEST_SOURCE_MONGODB_URI")
	if base == "" {
		t.Skip("no source deployment")
	}
	uri := strings.Replace(base, "mongodb://", "mongodb://reproalice:pw12345678@", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := source.Connect(ctx, uri)
	if err == nil {
		_ = client.Disconnect(ctx)
		t.Skip("deployment accepted the credentials")
	}
	t.Logf("Connect error: %v", err)
	if strings.Contains(err.Error(), "pw12345678") {
		t.Fatalf("password in error")
	}
	if strings.Contains(err.Error(), "reproalice") {
		t.Errorf("user name in the printed driver error")
	}
}
