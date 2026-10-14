package app

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Test flow:
//  1. Start the republish loop with one escrow left unopened at boot and no devshard write to come.
//  2. Let the retry delay pass twice: the first retry still fails, the second opens the escrow.
//  3. Assert the loop retried on its own until the escrow opened, and stopped retrying after it did.
func TestRepublishRetriesAnUnopenedEscrowUntilItOpens(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var publishes atomic.Int32
		publish := func(context.Context) (int, error) {
			if publishes.Add(1) == 1 {
				return 1, nil
			}
			return 0, nil
		}

		done := republishEscrows(ctx, make(chan struct{}), 1, publish)
		time.Sleep(unopenedEscrowRetryDelay)
		synctest.Wait()
		afterFirstRetry := publishes.Load()
		time.Sleep(unopenedEscrowRetryDelay)
		synctest.Wait()
		afterSecondRetry := publishes.Load()
		time.Sleep(10 * unopenedEscrowRetryDelay)
		synctest.Wait()
		cancel()
		<-done

		if afterFirstRetry != 1 || afterSecondRetry != 2 {
			t.Fatalf("publishes after each retry delay = %d, %d; want 1, 2", afterFirstRetry, afterSecondRetry)
		}
		if got := publishes.Load(); got != 2 {
			t.Fatalf("publishes once every escrow opened = %d, want 2", got)
		}
	})
}

// Test flow:
//  1. Start the republish loop with every escrow open at boot.
//  2. Let time pass with no write, then signal one devshard write.
//  3. Assert nothing republished on a timer and the write republished once.
func TestRepublishWaitsForAWriteWhenEveryEscrowOpened(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var publishes atomic.Int32
		publish := func(context.Context) (int, error) {
			publishes.Add(1)
			return 0, nil
		}
		writes := make(chan struct{}, 1)

		done := republishEscrows(ctx, writes, 0, publish)
		time.Sleep(10 * unopenedEscrowRetryDelay)
		synctest.Wait()
		withoutWrite := publishes.Load()
		writes <- struct{}{}
		synctest.Wait()
		cancel()
		<-done

		if withoutWrite != 0 {
			t.Fatalf("publishes with no write and nothing unopened = %d, want 0", withoutWrite)
		}
		if got := publishes.Load(); got != 1 {
			t.Fatalf("publishes after one write = %d, want 1", got)
		}
	})
}
