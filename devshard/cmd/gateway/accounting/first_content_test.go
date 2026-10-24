package accounting

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func recordFirstContent(t *testing.T, book *Book, attempts ...Attempt) {
	t.Helper()
	recordFirstContentOn(t, book, testEscrow, attempts...)
}

func recordFirstContentOn(t *testing.T, book *Book, escrowID string, attempts ...Attempt) {
	t.Helper()
	for index := range attempts {
		attempts[index].RequestID = "request-1"
		attempts[index].Sent = true
		attempts[index].Usage = UsageLoser
	}
	if err := book.RecordRace(escrowID, attempts); err != nil {
		t.Fatalf("RecordRace(): %v", err)
	}
}

func recordOf(t *testing.T, book *Book, participant string) ParticipantRecord {
	t.Helper()
	for _, record := range book.Query(QueryFilter{}) {
		if record.Participant == participant {
			return record
		}
	}
	t.Fatalf("no record for participant %s", participant)
	return ParticipantRecord{}
}

func onlyEpoch(t *testing.T, book *Book) EpochSummary {
	t.Helper()
	epochs := book.Epochs(QueryFilter{})
	if len(epochs) != 1 {
		t.Fatalf("Epochs() = %d summaries, want 1", len(epochs))
	}
	return epochs[0]
}

func bucketsJSON(t *testing.T, buckets FirstContentBuckets) string {
	t.Helper()
	encoded, err := json.Marshal(buckets)
	if err != nil {
		t.Fatalf("encoding buckets %v: %v", buckets, err)
	}
	return string(encoded)
}

func assertReport(t *testing.T, level string, report firstContentReport, total, attempts uint64, buckets string, median, tail uint64) {
	t.Helper()
	if report.FirstContentMillisecondsTotal != total || report.FirstContentAttempts != attempts {
		t.Fatalf("%s first content = %d ms over %d attempts, want %d ms over %d",
			level, report.FirstContentMillisecondsTotal, report.FirstContentAttempts, total, attempts)
	}
	if got := bucketsJSON(t, report.FirstContentBuckets); got != buckets {
		t.Fatalf("%s first_content_buckets = %s, want %s", level, got, buckets)
	}
	if report.FirstContentP50Milliseconds != median || report.FirstContentP95Milliseconds != tail {
		t.Fatalf("%s p50/p95 = %d/%d ms, want %d/%d", level,
			report.FirstContentP50Milliseconds, report.FirstContentP95Milliseconds, median, tail)
	}
}

func timedBook(t *testing.T) *Book {
	t.Helper()
	book := newTestBook(t, 4)
	recordFirstContent(t, book,
		Attempt{Nonce: 5, FirstContentDelay: 300 * time.Millisecond},
		Attempt{Nonce: 9, FirstContentDelay: 500 * time.Millisecond},
		Attempt{Nonce: 6},
	)
	return book
}

// Test flow:
//  1. Record a race whose two attempts on slot 1 reached first content 300 ms and 500 ms after their sends, and whose attempt on slot 2 sent no content.
//  2. Assert slot 1's participant record carries 800 ms over 2 timed attempts, in the 355 ms and 555 ms buckets, p50 355 and p95 555.
//  3. Assert slot 2's participant record carries no timing.
//  4. Assert the epoch summary carries the same timing as slot 1.
func TestTheTimeToFirstContentReachesTheParticipantAndTheEpoch(t *testing.T) {
	book := timedBook(t)

	assertReport(t, "slot 1", recordOf(t, book, participantFor(1)).firstContentReport, 800, 2, `{"355":1,"555":1}`, 355, 555)
	assertReport(t, "slot 2", recordOf(t, book, participantFor(2)).firstContentReport, 0, 0, `null`, 0, 0)
	assertReport(t, "epoch", onlyEpoch(t, book).firstContentReport, 800, 2, `{"355":1,"555":1}`, 355, 555)
}

// Test flow:
//  1. For each delay, record one attempt with it on a fresh book.
//  2. Assert the participant's buckets hold exactly one attempt under the expected upper bound, the overflow key past the largest bound.
func TestADelayLandsInTheBucketOfTheSmallestBoundItDoesNotExceed(t *testing.T) {
	for _, test := range []struct {
		delay time.Duration
		want  string
	}{
		{time.Millisecond, `{"10":1}`},
		{10 * time.Millisecond, `{"10":1}`},
		{10*time.Millisecond + time.Microsecond, `{"13":1}`},
		{250 * time.Millisecond, `{"284":1}`},
		{284 * time.Millisecond, `{"284":1}`},
		{285 * time.Millisecond, `{"355":1}`},
		{2 * time.Second, `{"2118":1}`},
		{9 * time.Minute, `{"560519":1}`},
		{560519 * time.Millisecond, `{"560519":1}`},
		{560520 * time.Millisecond, `{"+Inf":1}`},
		{time.Hour, `{"+Inf":1}`},
	} {
		book := newTestBook(t, 4)
		recordFirstContent(t, book, Attempt{Nonce: 5, FirstContentDelay: test.delay})

		if got := bucketsJSON(t, recordOf(t, book, participantFor(1)).FirstContentBuckets); got != test.want {
			t.Fatalf("buckets after a %v delay = %s, want %s", test.delay, got, test.want)
		}
	}
}

// Test flow:
//  1. Record 100 timed attempts on slot 1: 50 at 50 ms, 44 at 200 ms and 6 at 2 s.
//  2. Assert the participant's p50 is the 60 ms bound, its p95 the 2118 ms bound, and its total and buckets hold all 100.
func TestThePercentilesAreTheUpperBoundOfTheBucketHoldingTheNearestRank(t *testing.T) {
	book := newTestBook(t, 4)
	var attempts []Attempt
	for _, group := range []struct {
		delay time.Duration
		count int
	}{{50 * time.Millisecond, 50}, {200 * time.Millisecond, 44}, {2 * time.Second, 6}} {
		for range group.count {
			attempts = append(attempts, Attempt{Nonce: uint64(1 + 4*len(attempts)), FirstContentDelay: group.delay})
		}
	}
	recordFirstContent(t, book, attempts...)

	assertReport(t, "participant", recordOf(t, book, participantFor(1)).firstContentReport,
		50*50+44*200+6*2000, 100, `{"2118":6,"227":44,"60":50}`, 60, 2118)
}

// Test flow:
//  1. Record one attempt past the largest bound.
//  2. Assert it is counted in the overflow bucket and both percentiles read as the largest bound.
func TestAnAttemptPastTheLargestBoundReadsAsTheLargestBound(t *testing.T) {
	book := newTestBook(t, 4)
	recordFirstContent(t, book, Attempt{Nonce: 5, FirstContentDelay: 20 * time.Minute})

	assertReport(t, "participant", recordOf(t, book, participantFor(1)).firstContentReport,
		uint64((20 * time.Minute).Milliseconds()), 1, `{"+Inf":1}`, 560519, 560519)
}

// Test flow:
//  1. Open a second escrow with the same participants in the same epoch.
//  2. Record 50 ms on slot 1 and 200 ms on slot 2 of the first escrow, and 50 ms and 2 s on slot 1 of the second.
//  3. Assert slot 1's participant sums its two escrows' buckets and the epoch sums both slots.
func TestTheBucketsRollUpAcrossSlotsAndEscrows(t *testing.T) {
	book := newTestBook(t, 4)
	openTestEscrow(t, book, otherEscrow, testEpoch, 4)
	recordFirstContent(t, book,
		Attempt{Nonce: 5, FirstContentDelay: 50 * time.Millisecond},
		Attempt{Nonce: 6, FirstContentDelay: 200 * time.Millisecond},
	)
	recordFirstContentOn(t, book, otherEscrow,
		Attempt{Nonce: 5, FirstContentDelay: 50 * time.Millisecond},
		Attempt{Nonce: 9, FirstContentDelay: 2 * time.Second},
	)

	participant := recordOf(t, book, participantFor(1))
	assertReport(t, "participant", participant.firstContentReport, 2100, 3, `{"2118":1,"60":2}`, 60, 2118)
	if len(participant.Slots) != 2 {
		t.Fatalf("participant slots = %d, want one per escrow", len(participant.Slots))
	}
	assertReport(t, "first escrow's slot", participant.Slots[0].firstContentReport, 50, 1, `{"60":1}`, 60, 60)
	assertReport(t, "epoch", onlyEpoch(t, book).firstContentReport, 2300, 4, `{"2118":1,"227":1,"60":2}`, 60, 2118)
}

// Test flow:
//  1. Record the same attempt with a 300 ms time to first content twice.
//  2. Assert its participant carries 300 ms over 1 attempt in one bucket.
func TestAnAttemptReportedTwiceIsTimedOnce(t *testing.T) {
	book := newTestBook(t, 4)

	for range 2 {
		recordFirstContent(t, book, Attempt{Nonce: 5, FirstContentDelay: 300 * time.Millisecond})
	}

	assertReport(t, "participant", recordOf(t, book, participantFor(1)).firstContentReport, 300, 1, `{"355":1}`, 355, 355)
}

// Test flow:
//  1. Take a snapshot and a query of a book with one timed attempt.
//  2. Record a second attempt in the same bucket.
//  3. Assert the snapshot and the queried slot still hold one attempt in that bucket.
func TestASnapshotAndAQueryDoNotShareTheLiveBuckets(t *testing.T) {
	book := newTestBook(t, 4)
	recordFirstContent(t, book, Attempt{Nonce: 5, FirstContentDelay: 300 * time.Millisecond})
	snapshot := book.Snapshot()
	queried := recordOf(t, book, participantFor(1))

	recordFirstContent(t, book, Attempt{Nonce: 9, FirstContentDelay: 300 * time.Millisecond})

	if got := bucketsJSON(t, snapshot.Escrows[0].FirstContent[1].FirstContentBuckets); got != `{"355":1}` {
		t.Fatalf("snapshot buckets = %s, want the one attempt it was taken with", got)
	}
	if got := bucketsJSON(t, queried.Slots[0].FirstContentBuckets); got != `{"355":1}` {
		t.Fatalf("queried slot buckets = %s, want the one attempt it was read with", got)
	}
}

// Test flow:
//  1. Record two timed attempts on slot 1 totalling 800 ms and one untimed attempt on slot 2.
//  2. Save and reload the book.
//  3. Assert slot 1's participant and the epoch still carry 800 ms over 2 attempts with their buckets and percentiles.
func TestTheTimeToFirstContentSurvivesARestart(t *testing.T) {
	restored := saveAndReload(t, timedBook(t), openTestStore(t))

	assertReport(t, "participant", recordOf(t, restored, participantFor(1)).firstContentReport, 800, 2, `{"355":1,"555":1}`, 355, 555)
	assertReport(t, "epoch", onlyEpoch(t, restored).firstContentReport, 800, 2, `{"355":1,"555":1}`, 355, 555)
}

// Test flow:
//  1. Record a 300 ms attempt and save, then record a 500 ms attempt and save again.
//  2. Reload the book.
//  3. Assert the participant carries both attempts once each.
func TestTwoSavesOfATimedEscrowReloadAsTheirSum(t *testing.T) {
	book := newTestBook(t, 4)
	store := openTestStore(t)
	recordFirstContent(t, book, Attempt{Nonce: 5, FirstContentDelay: 300 * time.Millisecond})
	if err := store.Save(context.Background(), book); err != nil {
		t.Fatalf("Save(): %v", err)
	}
	recordFirstContent(t, book, Attempt{Nonce: 9, FirstContentDelay: 500 * time.Millisecond})

	restored := saveAndReload(t, book, store)

	assertReport(t, "participant", recordOf(t, restored, participantFor(1)).firstContentReport, 800, 2, `{"355":1,"555":1}`, 355, 555)
}

func storedFirstContentRows(t *testing.T, db *sql.DB, escrowID string) int {
	t.Helper()
	total := 0
	for _, table := range firstContentTables {
		var rows int
		if err := db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE escrow_id = ?`, escrowID).Scan(&rows); err != nil {
			t.Fatalf("counting %s rows of %s: %v", table, escrowID, err)
		}
		total += rows
	}
	return total
}

// Test flow:
//  1. For a prune and for a reset: open a timed escrow-1 and an untimed escrow-2, save, and assert escrow-1 has first-content rows.
//  2. Retire and prune escrow-1, or reset its epoch, and save again.
//  3. Assert no first-content row of escrow-1 is left in the store.
func TestAnEscrowLeavingTheBookTakesItsFirstContentRowsWithIt(t *testing.T) {
	for name, remove := range map[string]func(*Book){
		"prune": func(book *Book) { book.RetireEscrow(testEscrow); book.PruneBefore(testEpoch + 5) },
		"reset": func(book *Book) { book.ResetEpoch(testEpoch) },
	} {
		book := timedBook(t)
		openTestEscrow(t, book, otherEscrow, testEpoch+5, 4)
		store := openTestStore(t)
		if err := store.Save(context.Background(), book); err != nil {
			t.Fatalf("%s: Save(): %v", name, err)
		}
		if rows := storedFirstContentRows(t, store.db, testEscrow); rows != 3 {
			t.Fatalf("%s: first-content rows before = %d, want 1 timing row and 2 bucket rows", name, rows)
		}

		remove(book)
		if err := store.Save(context.Background(), book); err != nil {
			t.Fatalf("%s: Save(): %v", name, err)
		}

		if rows := storedFirstContentRows(t, store.db, testEscrow); rows != 0 {
			t.Fatalf("%s: first-content rows after = %d, want none", name, rows)
		}
	}
}

func saveTimedStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "accounting.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore(): %v", err)
	}
	book := timedBook(t)
	if err := book.RecordGhost(testEscrow, 7, "participant_window_full_no_send"); err != nil {
		t.Fatalf("RecordGhost(): %v", err)
	}
	if err := store.Save(context.Background(), book); err != nil {
		t.Fatalf("Save(): %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	return path
}

func rewriteStore(t *testing.T, path string, statements ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("opening the store as another build left it: %v", err)
	}
	defer func() { _ = db.Close() }()
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func reopenStore(t *testing.T, path string) (*Store, *Book) {
	t.Helper()
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore() over the rewritten store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	snapshot, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if len(snapshot.Escrows) != 1 || len(snapshot.Escrows[0].Counters) == 0 {
		t.Fatalf("the reopened store holds %+v, want the escrow and counters it was saved with", snapshot.Escrows)
	}
	restored := NewBook(func() time.Time { return time.Unix(0, 0).UTC() })
	if err := restored.Restore(snapshot); err != nil {
		t.Fatalf("Restore(): %v", err)
	}
	return store, restored
}

// Test flow:
//  1. Save a book holding a ghost and two timed attempts, then drop both first-content tables and their meta key as a store written before them.
//  2. Reopen and load the store.
//  3. Assert the escrow and its counters are kept and the restored ledger carries no first-content timing.
func TestAStoreWrittenBeforeTheFirstContentTablesIsKeptRatherThanDropped(t *testing.T) {
	path := saveTimedStore(t)
	rewriteStore(t, path,
		`DROP TABLE accounting_first_content`,
		`DROP TABLE accounting_first_content_buckets`,
		`DELETE FROM accounting_meta WHERE key = 'first_content_written_at'`,
	)

	_, restored := reopenStore(t, path)

	assertReport(t, "participant", recordOf(t, restored, participantFor(1)).firstContentReport, 0, 0, `null`, 0, 0)
}

// Test flow:
//  1. Save a timed book, then move updated_at on as a save by a build without the first-content tables would.
//  2. Reopen and load the store.
//  3. Assert the escrow is kept, its timing starts at zero and no first-content row is left.
func TestFirstContentRowsAnotherBuildSavedAroundAreDiscarded(t *testing.T) {
	path := saveTimedStore(t)
	rewriteStore(t, path, `UPDATE accounting_meta SET value = '2001-01-01T00:00:00Z' WHERE key = 'updated_at'`)

	store, restored := reopenStore(t, path)

	assertReport(t, "participant", recordOf(t, restored, participantFor(1)).firstContentReport, 0, 0, `null`, 0, 0)
	if rows := storedFirstContentRows(t, store.db, testEscrow); rows != 0 {
		t.Fatalf("first-content rows = %d, want none", rows)
	}
}

// Test flow:
//  1. Save a timed book, then add first-content rows for an escrow the store does not hold, as a prune by a build without the tables leaves them.
//  2. Reopen and load the store.
//  3. Assert the stray rows are gone and the held escrow keeps its timing.
func TestFirstContentRowsOfAnEscrowTheStoreDoesNotHoldAreDiscarded(t *testing.T) {
	path := saveTimedStore(t)
	rewriteStore(t, path,
		`INSERT INTO accounting_first_content VALUES ('escrow-gone', 1, 300, 1)`,
		`INSERT INTO accounting_first_content_buckets VALUES ('escrow-gone', 1, 16, 1)`,
	)

	store, restored := reopenStore(t, path)

	if rows := storedFirstContentRows(t, store.db, "escrow-gone"); rows != 0 {
		t.Fatalf("stray first-content rows = %d, want none", rows)
	}
	assertReport(t, "participant", recordOf(t, restored, participantFor(1)).firstContentReport, 800, 2, `{"355":1,"555":1}`, 355, 555)
}

func decodeBody(t *testing.T, body []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(body, target); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
}

func assertServedFirstContent(t *testing.T, level string, object map[string]any, want map[string]any) {
	t.Helper()
	if object == nil {
		t.Fatalf("no %s object in the response", level)
	}
	for field, value := range want {
		got, present := object[field]
		if !present {
			t.Fatalf("%s carries no %s", level, field)
		}
		if gotJSON, wantJSON := mustJSON(t, got), mustJSON(t, value); gotJSON != wantJSON {
			t.Fatalf("%s %s = %s, want %s", level, field, gotJSON, wantJSON)
		}
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encoding %v: %v", value, err)
	}
	return string(encoded)
}

// Test flow:
//  1. Record two timed attempts on slot 1, 300 ms and 500 ms.
//  2. Request the current-epoch participants route and the epochs route.
//  3. Assert slot 1's participant object and the epoch object carry the total, the count, the buckets and both percentiles.
func TestTheSurfaceServesTheTimeToFirstContent(t *testing.T) {
	book := timedBook(t)
	want := map[string]any{
		"first_content_ms_total": 800,
		"first_content_attempts": 2,
		"first_content_buckets":  map[string]int{"355": 1, "555": 1},
		"first_content_p50_ms":   355,
		"first_content_p95_ms":   555,
	}

	var participants struct {
		Participants []map[string]any `json:"participants"`
	}
	decodeBody(t, serve(t, book, "/api/v1/epochs/current/participants").Body.Bytes(), &participants)
	var timed map[string]any
	for _, participant := range participants.Participants {
		if participant["participant"] == participantFor(1) {
			timed = participant
		}
	}
	assertServedFirstContent(t, "participant", timed, want)

	var epochs struct {
		Epochs []map[string]any `json:"epochs"`
	}
	decodeBody(t, serve(t, book, "/api/v1/epochs").Body.Bytes(), &epochs)
	if len(epochs.Epochs) != 1 {
		t.Fatalf("epochs route = %d summaries, want 1", len(epochs.Epochs))
	}
	assertServedFirstContent(t, "epoch", epochs.Epochs[0], want)
}

// Test flow:
//  1. Record a 300 ms, a 500 ms and a 20 minute attempt on slot 1 and request its participant's route.
//  2. Decode the response into the ledger's own record type.
//  3. Assert the decoded buckets are the served ones, overflow included.
//  4. Assert a bucket key that is not one of the bounds is refused.
func TestAServedRecordDecodesIntoTheLedgersOwnType(t *testing.T) {
	book := timedBook(t)
	recordFirstContent(t, book, Attempt{Nonce: 13, FirstContentDelay: 20 * time.Minute})

	records := decodeParticipants(t, serve(t, book, "/api/v1/epochs/current/participants/"+participantFor(1)))

	if len(records) != 1 {
		t.Fatalf("participant route = %d records, want 1", len(records))
	}
	if got := bucketsJSON(t, records[0].FirstContentBuckets); got != `{"+Inf":1,"355":1,"555":1}` {
		t.Fatalf("decoded buckets = %s, want the served ones", got)
	}
	var decoded FirstContentBuckets
	if err := json.Unmarshal([]byte(`{"356":1}`), &decoded); err == nil {
		t.Fatalf("decoding a key between two bounds = %v, want an error", decoded)
	}
}

// Test flow:
//  1. Request one participant's route over a book with no timed attempt.
//  2. Assert its record carries the total, the count and both percentiles at zero, and no buckets.
func TestTheSurfaceServesAZeroTimingRatherThanOmittingIt(t *testing.T) {
	book := newTestBook(t, 4)
	recordFirstContent(t, book, Attempt{Nonce: 5})

	recorder := serve(t, book, "/api/v1/epochs/current/participants/"+participantFor(1))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var body struct {
		Records []map[string]any `json:"records"`
	}
	decodeBody(t, recorder.Body.Bytes(), &body)
	if len(body.Records) != 1 {
		t.Fatalf("participant route = %d records, want 1", len(body.Records))
	}
	assertServedFirstContent(t, "participant", body.Records[0], map[string]any{
		"first_content_ms_total": 0,
		"first_content_attempts": 0,
		"first_content_p50_ms":   0,
		"first_content_p95_ms":   0,
	})
	if buckets, present := body.Records[0]["first_content_buckets"]; present {
		t.Fatalf("first_content_buckets = %v, want absent with no timed attempt", buckets)
	}
}
