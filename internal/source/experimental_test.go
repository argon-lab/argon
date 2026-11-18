package source

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSourceExperimentalOptions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		check   func() error
		message string
	}{
		{"write-back", RequireExperimentalWriteBack, "this command is experimental and off by default: it connects Argon to a database that is in use. Set ARGON_EXPERIMENTAL_SOURCE=1 to enable it, and read docs/OPERATIONS.md first"},
		{"auto-resync", RequireExperimentalAutoResync, "--auto-resync is experimental and off by default. Set ARGON_EXPERIMENTAL_SOURCE=1 to allow following to resync main automatically"},
		{"source images", RequireExperimentalSourceImages, "--enable-source-images is experimental and off by default. Set ARGON_EXPERIMENTAL_SOURCE=1 to use it, or enable changeStreamPreAndPostImages directly in MongoDB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const env = "ARGON_EXPERIMENTAL_SOURCE"
			t.Setenv(env, "")
			require.NoError(t, os.Unsetenv(env))
			require.EqualError(t, tc.check(), tc.message)
			for _, value := range []string{"", "0", "true", "01", " 1", "1 "} {
				t.Setenv(env, value)
				require.EqualError(t, tc.check(), tc.message, "value %q", value)
			}
			t.Setenv(env, "1")
			require.NoError(t, tc.check())
		})
	}
}
