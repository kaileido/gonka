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
	for index := range attempts {
		attempts[index].RequestID = "request-1"
		attempts[index].Sent = true
		attempts[index].Usage = UsageLoser
	}
	if err := book.RecordRace(testEscrow, attempts); err != nil {
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
//  2. Assert slot 1's participant record carries 800 ms over 2 timed attempts.
//  3. Assert slot 2's participant record carries no timing.
//  4. Assert the epoch summary carries the same 800 ms over 2 attempts.
func TestTheTimeToFirstContentReachesTheParticipantAndTheEpoch(t *testing.T) {
	book := timedBook(t)

	timed := recordOf(t, book, participantFor(1))
	if timed.FirstContentMilliseconds != 800 || timed.FirstContentAttempts != 2 {
		t.Fatalf("slot 1 first content = %d ms over %d attempts, want 800 ms over 2",
			timed.FirstContentMilliseconds, timed.FirstContentAttempts)
	}
	silent := recordOf(t, book, participantFor(2))
	if silent.FirstContentMilliseconds != 0 || silent.FirstContentAttempts != 0 {
		t.Fatalf("slot 2 first content = %d ms over %d attempts, want nothing from an attempt without content",
			silent.FirstContentMilliseconds, silent.FirstContentAttempts)
	}
	epochs := book.Epochs(QueryFilter{})
	if len(epochs) != 1 {
		t.Fatalf("Epochs() = %d summaries, want 1", len(epochs))
	}
	if epochs[0].FirstContentMilliseconds != 800 || epochs[0].FirstContentAttempts != 2 {
		t.Fatalf("epoch first content = %d ms over %d attempts, want 800 ms over 2",
			epochs[0].FirstContentMilliseconds, epochs[0].FirstContentAttempts)
	}
}

// Test flow:
//  1. Record the same attempt with a 300 ms time to first content three times.
//  2. Assert its participant carries 300 ms over 1 attempt.
func TestAnAttemptReportedTwiceIsTimedOnce(t *testing.T) {
	book := newTestBook(t, 4)

	for range 3 {
		recordFirstContent(t, book, Attempt{Nonce: 5, FirstContentDelay: 300 * time.Millisecond})
	}

	record := recordOf(t, book, participantFor(1))
	if record.FirstContentMilliseconds != 300 || record.FirstContentAttempts != 1 {
		t.Fatalf("first content = %d ms over %d attempts, want 300 ms over 1",
			record.FirstContentMilliseconds, record.FirstContentAttempts)
	}
}

// Test flow:
//  1. Record two timed attempts on slot 1 totalling 800 ms and one untimed attempt on slot 2.
//  2. Save and reload the book.
//  3. Assert slot 1's participant still carries 800 ms over 2 attempts.
//  4. Assert the epoch summary still carries 800 ms over 2 attempts.
func TestTheTimeToFirstContentSurvivesARestart(t *testing.T) {
	restored := saveAndReload(t, timedBook(t), openTestStore(t))

	record := recordOf(t, restored, participantFor(1))
	if record.FirstContentMilliseconds != 800 || record.FirstContentAttempts != 2 {
		t.Fatalf("first content after the restart = %d ms over %d attempts, want 800 ms over 2",
			record.FirstContentMilliseconds, record.FirstContentAttempts)
	}
	epochs := restored.Epochs(QueryFilter{})
	if len(epochs) != 1 || epochs[0].FirstContentAttempts != 2 {
		t.Fatalf("Epochs() after the restart = %+v, want one summary with 2 timed attempts", epochs)
	}
}

// Test flow:
//  1. Save a book holding a ghost and two timed attempts, then drop the first-content table as a store written before it.
//  2. Reopen and load the store.
//  3. Assert the escrow and its counters are kept.
//  4. Assert the restored ledger carries no first-content timing.
func TestAStoreWrittenBeforeTheFirstContentTableIsKeptRatherThanDropped(t *testing.T) {
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
	dropFirstContentTable(t, path)

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore() over the older store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	snapshot, err := reopened.Load(context.Background())
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}

	if len(snapshot.Escrows) != 1 || len(snapshot.Escrows[0].Counters) == 0 {
		t.Fatalf("the upgrade left %+v, want the escrow and counters the older store held", snapshot.Escrows)
	}
	restored := NewBook(func() time.Time { return time.Unix(0, 0).UTC() })
	if err := restored.Restore(snapshot); err != nil {
		t.Fatalf("Restore(): %v", err)
	}
	record := recordOf(t, restored, participantFor(1))
	if record.FirstContentMilliseconds != 0 || record.FirstContentAttempts != 0 {
		t.Fatalf("first content = %d ms over %d attempts, want none from a store without the table",
			record.FirstContentMilliseconds, record.FirstContentAttempts)
	}
}

func dropFirstContentTable(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("opening the store as the older build left it: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`DROP TABLE accounting_first_content`); err != nil {
		t.Fatalf("dropping the first-content table: %v", err)
	}
}

// Test flow:
//  1. Record two timed attempts on slot 1 totalling 800 ms.
//  2. Request the current-epoch participants route and the epochs route.
//  3. Assert slot 1's participant object carries first_content_ms_total 800 and first_content_attempts 2.
//  4. Assert the epoch object carries the same two fields.
func TestTheSurfaceServesTheTimeToFirstContent(t *testing.T) {
	book := timedBook(t)

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
	assertFirstContentFields(t, "participant", timed)

	var epochs struct {
		Epochs []map[string]any `json:"epochs"`
	}
	decodeBody(t, serve(t, book, "/api/v1/epochs").Body.Bytes(), &epochs)
	if len(epochs.Epochs) != 1 {
		t.Fatalf("epochs route = %d summaries, want 1", len(epochs.Epochs))
	}
	assertFirstContentFields(t, "epoch", epochs.Epochs[0])
}

func decodeBody(t *testing.T, body []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(body, target); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
}

func assertFirstContentFields(t *testing.T, level string, object map[string]any) {
	t.Helper()
	if object == nil {
		t.Fatalf("no %s object in the response", level)
	}
	if got := object["first_content_ms_total"]; got != float64(800) {
		t.Fatalf("%s first_content_ms_total = %v, want 800", level, got)
	}
	if got := object["first_content_attempts"]; got != float64(2) {
		t.Fatalf("%s first_content_attempts = %v, want 2", level, got)
	}
}

// Test flow:
//  1. Build a participants response over a book with no timed attempt.
//  2. Assert the participant object still carries both first-content fields, at zero.
func TestTheSurfaceServesAZeroTimingRatherThanOmittingIt(t *testing.T) {
	book := newTestBook(t, 4)
	recordFirstContent(t, book, Attempt{Nonce: 5})

	recorder := serve(t, book, "/api/v1/epochs/current/participants/"+participantFor(1))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var body struct {
		Participants []map[string]any `json:"participants"`
		Records      []map[string]any `json:"records"`
	}
	decodeBody(t, recorder.Body.Bytes(), &body)
	objects := append(body.Participants, body.Records...)
	if len(objects) != 1 {
		t.Fatalf("participant route = %d objects, want 1", len(objects))
	}
	for _, field := range []string{"first_content_ms_total", "first_content_attempts"} {
		if got, present := objects[0][field]; !present || got != float64(0) {
			t.Fatalf("%s = %v (present %v), want 0", field, got, present)
		}
	}
}
