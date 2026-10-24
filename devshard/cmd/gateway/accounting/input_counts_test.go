package accounting

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"devshard/cmd/gateway/filters"
	"devshard/types"
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

// Test flow:
//  1. Record a race whose nonces carry an exact tokenizer count, no count, and a sampled count, and observe them finished, finished and timed out.
//  2. Read the slot money live, after a save and reload, after the chain seals them, and after one more save and reload.
//  3. Assert each time the counted nonces are estimated by their tokenizer count, marked tokenized, and the uncounted one by its bytes.
//  4. Assert no input count is kept once their costs are sealed.
func TestARacedNonceIsEstimatedByItsTokenizerCountAndTheRestByTheirBytes(t *testing.T) {
	book := newTestBook(t, 4)
	recordCountedRace(t, book)
	if err := book.ObserveInferences(testEscrow, countedRaceInferences()); err != nil {
		t.Fatalf("ObserveInferences(): %v", err)
	}
	expected := map[string]SlotMoney{
		participantFor(0): {},
		participantFor(1): {Input: 100, EstimatedInput: 37, TokenizedInput: 37, CountedNonces: 1},
		participantFor(2): {Input: 200, EstimatedInput: filters.EstimatedPromptTokens(800), CountedNonces: 1},
		participantFor(3): {Reserved: 700, Refunded: 700, EstimatedError: 290, TokenizedError: 290},
	}
	store := openTestStore(t)
	restored := saveAndReload(t, book, store)
	if err := restored.ObserveInferences(testEscrow, map[uint64]*types.InferenceRecord{}); err != nil {
		t.Fatalf("ObserveInferences(): %v", err)
	}
	sealed := saveAndReload(t, restored, store)

	for name, book := range map[string]*Book{"live": book, "reloaded": restored, "sealed and reloaded": sealed} {
		for participant, money := range slotTotals(book.Query(QueryFilter{})) {
			if money != expected[participant] {
				t.Fatalf("%s: %s holds %+v, want %+v", name, participant, money, expected[participant])
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
		if err := book.ObserveInferences(testEscrow, inferences); err != nil {
			t.Fatalf("ObserveInferences(): %v", err)
		}
	}

	if err := book.RecordRace(testEscrow, []Attempt{{Nonce: 5, Sent: true, InputTokens: 37, InputTokensSource: filters.InputTokensTokenizerExact}}); err != nil {
		t.Fatalf("RecordRace(): %v", err)
	}
	if err := book.ObserveInferences(testEscrow, map[uint64]*types.InferenceRecord{}); err != nil {
		t.Fatalf("ObserveInferences(): %v", err)
	}

	money := moneyOf(t, book)
	if money.EstimatedInput != filters.EstimatedPromptTokens(400) || money.TokenizedInput != 0 {
		t.Fatalf("money = %+v, want the byte estimate the seal folded", money)
	}
	if counts := book.Snapshot().Escrows[0].InputCounts; len(counts) != 0 {
		t.Fatalf("input counts = %+v, want none for a sealed nonce", counts)
	}
}
