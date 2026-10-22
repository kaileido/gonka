package accounting

import (
	"testing"

	"github.com/stretchr/testify/require"

	"devshard/types"
)

// Test flow:
//  1. Assign nonce 4 and nonce 5; report nonce 4's race as won and record an execution timeout on nonce 5.
//  2. Observe the chain holding nonce 4 finished and nonce 5 started, then observe both sealed away.
//  3. Assert the counters a query reads are the same before and after the seal.
//  4. Assert the sealed, finished nonce 4 holds no row while the revisable nonce 5 keeps its row and stays listed unfinished.
func TestASealedNonceWithAFinalDispositionLeavesNoRow(t *testing.T) {
	book := newTestBook(t, 4)
	require.NoError(t, book.RecordAssigned(testEscrow, 4, "req-a"))
	require.NoError(t, book.RecordAssigned(testEscrow, 5, "req-b"))
	require.NoError(t, book.RecordRace(testEscrow, []Attempt{{Nonce: 4, RequestID: "req-a", Sent: true, Usage: UsageWinner, Terminal: "won"}}))
	require.NoError(t, book.RecordTimeout(testEscrow, 5, "execution", "completed", "none"))
	require.NoError(t, book.ObserveInferences(testEscrow, map[uint64]*types.InferenceRecord{
		4: {Status: types.StatusFinished},
		5: {Status: types.StatusStarted},
	}))
	beforeSeal := book.Query(QueryFilter{})

	require.NoError(t, book.ObserveInferences(testEscrow, map[uint64]*types.InferenceRecord{}))
	afterSeal := book.Query(QueryFilter{})

	require.Equal(t, beforeSeal, afterSeal)
	require.False(t, holdsNonceRow(book, testEscrow, 4))
	require.True(t, holdsNonceRow(book, testEscrow, 5))
	require.Equal(t, []uint64{5}, book.UnfinishedNonces(testEscrow))
}

func holdsNonceRow(book *Book, escrowID string, nonce uint64) bool {
	book.mu.RLock()
	defer book.mu.RUnlock()
	_, held := book.escrows[escrowID].nonces[nonce]
	return held
}
