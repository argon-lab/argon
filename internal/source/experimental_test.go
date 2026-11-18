package source

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequireExperimental(t *testing.T) {
	const name = "ARGON_EXPERIMENTAL_SOURCE"
	t.Setenv(name, "")
	require.NoError(t, os.Unsetenv(name))
	require.ErrorIs(t, RequireExperimental(), errExperimentalSource)

	for _, value := range []string{"", "0", "true", "01", " 1", "1 "} {
		t.Setenv(name, value)
		require.ErrorIs(t, RequireExperimental(), errExperimentalSource, "value %q", value)
	}
	t.Setenv(name, "1")
	require.NoError(t, RequireExperimental())
}
