package escrow

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"devshard/cmd/gateway/config"
)

type recordingSweeper struct {
	mu      sync.Mutex
	calls   int
	grace   time.Duration
	budget  int
	blockOn chan struct{}
}

func (s *recordingSweeper) SweepExecutionTimeouts(_ context.Context, grace time.Duration, budgetPerEscrow int) (int, int, int) {
	s.mu.Lock()
	s.calls++
	s.grace, s.budget = grace, budgetPerEscrow
	block := s.blockOn
	s.mu.Unlock()
	if block != nil {
		<-block
	}
	return 1, 1, 0
}

func (s *recordingSweeper) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func managerWithSweeper(t *testing.T, sweeper TimeoutSweeper, cfg *config.Config) *Manager {
	t.Helper()
	deps := testManagerDeps(t, newFakeStore(), &fakeTxClient{}, &fakeSnapshotSource{}, cfg)
	deps.Timeouts = sweeper
	return mustManager(t, deps)
}

// Test flow:
//  1. Configure a budget of 3 and a grace of 90 seconds, and build a manager with a `recordingSweeper`.
//  2. Call sweepTimeouts and wait for the sweep work to finish.
//  3. Assert the sweeper was called once with the configured grace and budget.
func TestSweepPassesTheConfiguredBudgetAndGrace(t *testing.T) {
	cfg := config.Defaults()
	cfg.TimeoutSweep.BudgetPerTick = 3
	cfg.TimeoutSweep.GraceSeconds = 90
	sweeper := &recordingSweeper{}
	manager := managerWithSweeper(t, sweeper, &cfg)

	manager.sweepTimeouts(context.Background())
	manager.offTickWork.Wait()

	require.Equal(t, 1, sweeper.callCount())
	require.Equal(t, 90*time.Second, sweeper.grace)
	require.Equal(t, 3, sweeper.budget)
}

// Test flow:
//  1. Configure a budget of 0 and build a manager with a `recordingSweeper`.
//  2. Call sweepTimeouts and wait for the sweep work to finish.
//  3. Assert the sweeper was never called.
func TestSweepIsOffWithoutABudget(t *testing.T) {
	cfg := config.Defaults()
	cfg.TimeoutSweep.BudgetPerTick = 0
	sweeper := &recordingSweeper{}
	manager := managerWithSweeper(t, sweeper, &cfg)

	manager.sweepTimeouts(context.Background())
	manager.offTickWork.Wait()

	require.Zero(t, sweeper.callCount())
}

// Test flow:
//  1. Build a manager with a `recordingSweeper` that blocks on a channel until released.
//  2. Call sweepTimeouts once and wait for the sweeper's first call to register.
//  3. Call sweepTimeouts a second time while the first sweep is still blocked, then release the block.
//  4. Wait for the sweep work to finish and assert the sweeper was called only once.
func TestASecondTickDoesNotStartASecondSweep(t *testing.T) {
	cfg := config.Defaults()
	cfg.TimeoutSweep.BudgetPerTick = 2
	release := make(chan struct{})
	sweeper := &recordingSweeper{blockOn: release}
	manager := managerWithSweeper(t, sweeper, &cfg)

	manager.sweepTimeouts(context.Background())
	require.Eventually(t, func() bool { return sweeper.callCount() == 1 }, time.Second, time.Millisecond)
	manager.sweepTimeouts(context.Background())
	close(release)
	manager.offTickWork.Wait()

	require.Equal(t, 1, sweeper.callCount())
}

// Test flow:
//  1. Build a manager with a `recordingSweeper` that blocks on a channel until released.
//  2. Call sweepTimeouts and wait for the sweeper's call to register.
//  3. Call Stop in a goroutine and assert it has not returned after 20ms, while the sweep is still blocked.
//  4. Release the block and assert Stop then returns.
func TestStopWaitsForASweepInFlight(t *testing.T) {
	cfg := config.Defaults()
	cfg.TimeoutSweep.BudgetPerTick = 2
	release := make(chan struct{})
	sweeper := &recordingSweeper{blockOn: release}
	manager := managerWithSweeper(t, sweeper, &cfg)
	manager.sweepTimeouts(context.Background())
	require.Eventually(t, func() bool { return sweeper.callCount() == 1 }, time.Second, time.Millisecond)

	stopped := make(chan struct{})
	go func() {
		manager.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("Stop() returned while a sweep was still voting")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-stopped
}
