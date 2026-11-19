package merge

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteBack_DeadlinePrecedesDetach(t *testing.T) {
	// Even a commit sent just as the writer times out has a bounded server
	// execution time. Detach allows extra time beyond both budgets.
	require.Greater(t, writeBackClaimWindow, 2*writeBackDeadline)
}
