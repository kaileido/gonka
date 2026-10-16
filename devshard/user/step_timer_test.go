package user

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"devshard/storage"
)

type recordedSteps struct {
	mu    sync.Mutex
	steps map[string]int
}

func (r *recordedSteps) record(step string, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps[step]++
}

func (r *recordedSteps) seen() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.steps
}

// Test flow:
//  1. Open a stored session with a step timer.
//  2. Serve one inference the way the gateway does: prepare, send, apply the response, then close.
//  3. Assert every lock and store step was timed: each one a request's throughput depends on is visible without a profiler.
func TestSession_AStepTimerSeesTheLockAndTheStore(t *testing.T) {
	session := setupStoredSession(t, storage.NewMemory())
	steps := &recordedSteps{steps: map[string]int{}}
	session.SetStepTimer(steps.record)

	prepared, err := session.PrepareInferenceFn(func(HostBinding) (InferenceParams, bool, context.Context, error) {
		return signatureTestParams(), false, nil, nil
	})
	require.NoError(t, err)
	reply, err := session.SendOnly(context.Background(), prepared, nil, nil)
	require.NoError(t, err)
	require.NoError(t, session.ProcessResponse(prepared.HostIdx(), reply, prepared.Nonce()))
	require.NoError(t, session.Close())

	seen := steps.seen()
	for _, step := range []string{StepLockWaitPrepare, StepLockHeldPrepare, StepStoreAppendDiff, StepLockWaitProcess, StepLockHeldProcess, StepStoreAddSignature} {
		require.Positive(t, seen[step], "step %s was never timed; timed: %v", step, seen)
	}
}
