package accounting

import (
	"context"
	"database/sql"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"devshard/cmd/gateway/filters"
	"devshard/types"
)

const (
	retiredCountedNonces       = 50_000
	retiredCountsHeapAllowance = 256 * weightBytesPerKibibyte
)

func countedRaceInferences() map[uint64]*types.InferenceRecord {
	return map[uint64]*types.InferenceRecord{
		5: {Status: types.StatusFinished, ExecutorSlot: 1, InputLength: 400, InputTokens: 100},
		6: {Status: types.StatusFinished, ExecutorSlot: 2, InputLength: 800, InputTokens: 200},
		7: {Status: types.StatusTimedOut, ExecutorSlot: 3, InputLength: 1200, ReservedCost: 700},
	}
}

func recordCountedRace(t *testing.T, book *Book) {
	t.Helper()
	if err := book.RecordRace(testEscrow, []Attempt{
		{Nonce: 5, Sent: true, InputTokens: 37, InputTokensSource: filters.InputTokensTokenizerExact},
		{Nonce: 6, Sent: true},
		{Nonce: 7, Sent: true, InputTokens: 290, InputTokensSource: filters.InputTokensTokenizerSampled},
	}); err != nil {
		t.Fatalf("RecordRace(): %v", err)
	}
}

func sweep(t *testing.T, book *Book, latestNonce uint64, inferences map[uint64]*types.InferenceRecord) {
	t.Helper()
	if err := book.ObserveLatestNonce(testEscrow, latestNonce); err != nil {
		t.Fatalf("ObserveLatestNonce(): %v", err)
	}
	if err := book.ObserveInferences(testEscrow, inferences); err != nil {
		t.Fatalf("ObserveInferences(): %v", err)
	}
}

// Test flow:
//  1. Record a race whose nonces carry an exact tokenizer count, no count, and a sampled count, and observe them finished, finished and timed out.
//  2. Read the slot money live, after a save and reload, after the chain seals them, and after one more save and reload.
//  3. Assert each time the counted nonces are estimated by their tokenizer count, marked tokenized, and the uncounted one by its bytes.
//  4. Assert no input count is kept once their costs are sealed.
func TestARacedNonceIsEstimatedByItsTokenizerCountAndTheRestByTheirBytes(t *testing.T) {
	live := newTestBook(t, 4)
	recordCountedRace(t, live)
	sweep(t, live, 7, countedRaceInferences())
	expected := map[string]SlotMoney{
		participantFor(0): {},
		participantFor(1): {Input: 100, EstimatedInput: 37, TokenizedInput: 37, CountedNonces: 1},
		participantFor(2): {Input: 200, EstimatedInput: filters.EstimatedPromptTokens(800), CountedNonces: 1},
		participantFor(3): {Reserved: 700, Refunded: 700, EstimatedError: 290, TokenizedError: 290},
	}
	store := openTestStore(t)
	totals := map[string]map[string]SlotMoney{"live": slotTotals(live.Query(QueryFilter{}))}
	restored := saveAndReload(t, live, store)
	totals["reloaded"] = slotTotals(restored.Query(QueryFilter{}))
	sweep(t, restored, 7, map[uint64]*types.InferenceRecord{})
	totals["sealed"] = slotTotals(restored.Query(QueryFilter{}))
	sealed := saveAndReload(t, restored, store)
	totals["sealed and reloaded"] = slotTotals(sealed.Query(QueryFilter{}))

	for name, money := range totals {
		for participant, slot := range money {
			if slot != expected[participant] {
				t.Fatalf("%s: %s holds %+v, want %+v", name, participant, slot, expected[participant])
			}
		}
	}
	if counts := sealed.Snapshot().Escrows[0].InputCounts; len(counts) != 0 {
		t.Fatalf("sealed escrow keeps input counts %+v, want none", counts)
	}
}

// Test flow:
//  1. Save a book with a finished inference and a sealed one, then strip the store to the shape before input counts: no tokenized money columns and no input count table.
//  2. Reopen the store, load and restore it, record a tokenizer-counted race on a new nonce and save it again.
//  3. Assert the stored nonces read back as byte estimates with nothing tokenized, and the new nonce's count was saved beside them.
func TestAStoreWrittenBeforeInputCountsReadsAsByteEstimates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounting.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore(): %v", err)
	}
	book := newTestBook(t, 4)
	for _, inferences := range []map[uint64]*types.InferenceRecord{
		{5: {Status: types.StatusFinished, InputLength: 400}, 6: {Status: types.StatusFinished, InputLength: 800}},
		{6: {Status: types.StatusFinished, InputLength: 800}},
	} {
		if err := book.ObserveInferences(testEscrow, inferences); err != nil {
			t.Fatalf("ObserveInferences(): %v", err)
		}
	}
	if err := store.Save(context.Background(), book); err != nil {
		t.Fatalf("Save(): %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	stripInputCounts(t, path)

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore() over the older store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restored := saveAndReload(t, book, reopened)
	if err := restored.RecordRace(testEscrow, []Attempt{{Nonce: 9, Sent: true, InputTokens: 11, InputTokensSource: filters.InputTokensTokenizerExact}}); err != nil {
		t.Fatalf("RecordRace(): %v", err)
	}
	reloaded := saveAndReload(t, restored, reopened)

	money := moneyOf(t, reloaded)
	if want := filters.EstimatedPromptTokens(400) + filters.EstimatedPromptTokens(800); money.EstimatedInput != want || money.TokenizedInput != 0 || money.CountedNonces != 2 {
		t.Fatalf("money = %+v, want %d estimated from bytes over 2 nonces, none tokenized", money, want)
	}
	if counts := reloaded.Snapshot().Escrows[0].InputCounts; len(counts) != 1 || counts[0] != (PersistedInputCount{Nonce: 9, Tokens: 11, Source: filters.InputTokensTokenizerExact}) {
		t.Fatalf("input counts = %+v, want nonce 9's 11 tokenizer tokens", counts)
	}
}

func stripInputCounts(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("opening the store as the older build left it: %v", err)
	}
	defer func() { _ = db.Close() }()
	for _, statement := range []string{
		`ALTER TABLE accounting_money DROP COLUMN tokenized_input`,
		`ALTER TABLE accounting_money DROP COLUMN tokenized_error`,
		`DROP TABLE accounting_input_counts`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// Test flow:
//  1. Observe a nonce finished before its race reported, then observe the escrow without it, as the chain seals it.
//  2. Record its race with a tokenizer count, and sweep the escrow once more.
//  3. Assert the sealed nonce keeps the byte estimate it was folded with and no input count is held for it.
func TestARaceReportedAfterItsNonceWasSealedKeepsNoCount(t *testing.T) {
	book := newTestBook(t, 4)
	for _, inferences := range []map[uint64]*types.InferenceRecord{{5: {Status: types.StatusFinished, InputLength: 400}}, {}} {
		sweep(t, book, 5, inferences)
	}

	if err := book.RecordRace(testEscrow, []Attempt{{Nonce: 5, Sent: true, InputTokens: 37, InputTokensSource: filters.InputTokensTokenizerExact}}); err != nil {
		t.Fatalf("RecordRace(): %v", err)
	}
	sweep(t, book, 5, map[uint64]*types.InferenceRecord{})

	money := moneyOf(t, book)
	if money.EstimatedInput != filters.EstimatedPromptTokens(400) || money.TokenizedInput != 0 {
		t.Fatalf("money = %+v, want the byte estimate the seal folded", money)
	}
	if counts := book.Snapshot().Escrows[0].InputCounts; len(counts) != 0 {
		t.Fatalf("input counts = %+v, want none for a sealed nonce", counts)
	}
}

// Test flow:
//  1. Record a tokenizer count for a nonce the escrow's sweeps have not seen yet.
//  2. Sweep the escrow at a watermark below that nonce, then at one that holds it finished, then once more with it sealed.
//  3. Assert the nonce was folded with its tokenizer count and no input count is left behind.
func TestACountRecordedBeforeTheSweepSeesItsNonceOutlivesThatSweep(t *testing.T) {
	book := newTestBook(t, 4)
	if err := book.RecordRace(testEscrow, []Attempt{{Nonce: 6, Sent: true, InputTokens: 37, InputTokensSource: filters.InputTokensTokenizerExact}}); err != nil {
		t.Fatalf("RecordRace(): %v", err)
	}
	finished := &types.InferenceRecord{Status: types.StatusFinished, InputLength: 400}
	sweep(t, book, 5, map[uint64]*types.InferenceRecord{5: finished})
	sweep(t, book, 6, map[uint64]*types.InferenceRecord{5: finished, 6: {Status: types.StatusFinished, InputLength: 4_000}})
	sweep(t, book, 6, map[uint64]*types.InferenceRecord{})

	money := moneyOf(t, book)
	if money.EstimatedInput != filters.EstimatedPromptTokens(400)+37 || money.TokenizedInput != 37 {
		t.Fatalf("money = %+v, want nonce 6 folded at its 37 tokenizer tokens", money)
	}
	if counts := book.Snapshot().Escrows[0].InputCounts; len(counts) != 0 {
		t.Fatalf("input counts = %+v, want none once both nonces are sealed", counts)
	}
}

// Test flow:
//  1. Start a sweep that reads a watermark holding a just-assigned nonce, before it observes the nonce's cost.
//  2. File the nonce's tokenizer count at assignment in between, then let the sweep observe the nonce pending and a later sweep observe it sealed.
//  3. Assert the nonce was folded with its tokenizer count.
func TestACountFiledWhileASweepIsBetweenItsWatermarkAndItsCostsIsKept(t *testing.T) {
	book := newTestBook(t, 4)
	if err := book.ObserveLatestNonce(testEscrow, 6); err != nil {
		t.Fatalf("ObserveLatestNonce(): %v", err)
	}
	if err := book.RecordInputCount(testEscrow, 6, 37, filters.InputTokensTokenizerExact); err != nil {
		t.Fatalf("RecordInputCount(): %v", err)
	}
	if err := book.ObserveInferences(testEscrow, map[uint64]*types.InferenceRecord{6: {Status: types.StatusPending, InputLength: 4_000}}); err != nil {
		t.Fatalf("ObserveInferences(): %v", err)
	}
	sweep(t, book, 6, map[uint64]*types.InferenceRecord{6: {Status: types.StatusFinished, InputLength: 4_000}})
	sweep(t, book, 6, map[uint64]*types.InferenceRecord{})

	if money := moneyOf(t, book); money.EstimatedInput != 37 || money.TokenizedInput != 37 {
		t.Fatalf("money = %+v, want nonce 6 folded at its 37 tokenizer tokens", money)
	}
}

// Test flow:
//  1. Record a race whose nonce carries a tokenizer count and save the book.
//  2. Sweep the escrow at a watermark holding that nonce with nothing live, so the sweep changes nothing but the count it drops.
//  3. Save and reload, and assert the dropped count left the store too.
func TestACountDroppedByASweepLeavesTheStore(t *testing.T) {
	book := newTestBook(t, 4)
	if err := book.RecordRace(testEscrow, []Attempt{{Nonce: 5, Sent: true, InputTokens: 37, InputTokensSource: filters.InputTokensTokenizerExact}}); err != nil {
		t.Fatalf("RecordRace(): %v", err)
	}
	store := openTestStore(t)
	if err := store.Save(t.Context(), book); err != nil {
		t.Fatalf("Save(): %v", err)
	}
	sweep(t, book, 5, map[uint64]*types.InferenceRecord{})
	if counts := book.Snapshot().Escrows[0].InputCounts; len(counts) != 0 {
		t.Fatalf("input counts in the book = %+v, want none once the sweep passed nonce 5", counts)
	}

	reloaded := saveAndReload(t, book, store)

	if counts := reloaded.Snapshot().Escrows[0].InputCounts; len(counts) != 0 {
		t.Fatalf("input counts in the store = %+v, want none once the sweep passed nonce 5", counts)
	}
}

// Test flow:
//  1. Record a tokenizer count for each of 50,000 nonces of one escrow.
//  2. Sweep the escrow at a watermark holding them all with nothing live, so every count is dropped, and retire it.
//  3. Assert the retired escrow keeps less than 256 KiB of heap after a collection.
func TestARetiredEscrowFreesTheCountsItDropped(t *testing.T) {
	before := heapInUse()
	book := NewBook(func() time.Time { return time.Unix(1700000000, 0).UTC() })
	openTestEscrow(t, book, testEscrow, testEpoch, 4)
	for nonce := uint64(1); nonce <= retiredCountedNonces; nonce++ {
		if err := book.RecordInputCount(testEscrow, nonce, 37, filters.InputTokensTokenizerExact); err != nil {
			t.Fatalf("RecordInputCount(): %v", err)
		}
	}
	sweep(t, book, retiredCountedNonces, map[uint64]*types.InferenceRecord{})
	book.RetireEscrow(testEscrow)

	retained := int64(heapInUse()) - int64(before)
	runtime.KeepAlive(book)

	if retained >= retiredCountsHeapAllowance {
		t.Fatalf("retired escrow keeps %d bytes of heap, want under %d once its counts are dropped", retained, retiredCountsHeapAllowance)
	}
}
