package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"devshard/cmd/gateway/chain"
	"devshard/cmd/gateway/escrow"
	"devshard/cmd/gateway/internal/logcapture"
	"devshard/cmd/gateway/registry"
	"devshard/cmd/gateway/store"
)

// guardedTree lays out a base storage dir and a sibling directory nothing may reach.
func guardedTree(t *testing.T) (base, guarded string) {
	t.Helper()
	root := t.TempDir()
	base = filepath.Join(root, "gateway")
	guarded = filepath.Join(root, "victim")
	for _, dir := range []string{filepath.Join(base, "escrow-7"), guarded} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("preparing %s: %v", dir, err)
		}
	}
	return base, guarded
}

// Test flow:
//  1. Build a base storage tree and a sibling `guarded` directory outside it.
//  2. Define a table of target paths, varying across an in-base escrow path, parent traversal, an absolute path outside the base, a URL-encoded traversal, and a sibling directory sharing the base's prefix.
//  3. For each case, call `removeDevshardStorage` on the target and the base.
//  4. Assert the in-base case removes cleanly, and every other case fails with the guarded directory left untouched.
func TestRemoveDevshardStorageRefusesAPathOutsideItsBase(t *testing.T) {
	testCases := []struct {
		name       string
		target     func(base, guarded string) string
		wantRemove bool
	}{
		{
			name:       "a path inside the base is removed",
			target:     func(base, _ string) string { return filepath.Join(base, "escrow-7") },
			wantRemove: true,
		},
		{
			name:   "parent traversal",
			target: func(base, _ string) string { return base + "/../victim" },
		},
		{
			name:   "absolute path",
			target: func(_, guarded string) string { return guarded },
		},
		{
			name:   "url encoded traversal",
			target: func(base, _ string) string { return base + "/" + mustUnescape(t, "%2e%2e%2fvictim") },
		},
		{
			name:   "a sibling sharing the base's prefix",
			target: func(base, _ string) string { return base + "-other" },
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			base, guarded := guardedTree(t)
			err := removeDevshardStorage(testCase.target(base, guarded), base)

			if testCase.wantRemove {
				if err != nil {
					t.Fatalf("removing an in-base path: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("removeDevshardStorage accepted a path outside %q", base)
			}
			if _, statErr := os.Stat(guarded); statErr != nil {
				t.Fatalf("the guarded directory was deleted: %v", statErr)
			}
		})
	}
}

// Test flow:
//  1. Build a base storage tree and a sibling `guarded` directory outside it.
//  2. Define a table of escrow IDs, varying across parent segments after a real one, repeated parent segments, and URL-encoded parent segments.
//  3. For each case, resolve the escrow ID through `DevshardStoragePath` and call `removeDevshardStorage`.
//  4. Assert every case fails and the guarded directory survives.
func TestAnEscrowIdCannotEscapeTheBaseStorageDir(t *testing.T) {
	testCases := []struct {
		name     string
		escrowID string
	}{
		{name: "parent segments after a real one", escrowID: "7/../../victim"},
		{name: "repeated parent segments", escrowID: "a/../../.."},
		{name: "url encoded parent segments", escrowID: mustUnescape(t, "7%2f%2e%2e%2f%2e%2e%2fvictim")},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			base, guarded := guardedTree(t)
			err := removeDevshardStorage(DevshardStoragePath(base, testCase.escrowID), base)
			if err == nil {
				t.Fatalf("escrow id %q reached %q", testCase.escrowID, filepath.Clean(DevshardStoragePath(base, testCase.escrowID)))
			}
			if _, statErr := os.Stat(guarded); statErr != nil {
				t.Fatalf("the guarded directory was deleted: %v", statErr)
			}
		})
	}
}

// Test flow:
//  1. Call `removeDevshardStorage` with the base directory as both its own target and base.
//  2. Assert it fails.
//  3. Assert the base directory still exists.
func TestRemoveDevshardStorageRefusesTheBaseDirectoryItself(t *testing.T) {
	base := t.TempDir()
	if err := removeDevshardStorage(base, base); err == nil {
		t.Fatal("removeDevshardStorage deleted its own base directory")
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("the base directory was deleted: %v", err)
	}
}

// Test flow:
//  1. Create an escrow directory holding a `state.db` file.
//  2. Call `removeDevshardStorage` on the state file's path.
//  3. Assert it succeeds and removes the whole escrow directory.
func TestRemoveDevshardStorageTreatsAStateFileAsItsDirectory(t *testing.T) {
	base := t.TempDir()
	escrowDir := filepath.Join(base, "escrow-7")
	if err := os.MkdirAll(escrowDir, 0o700); err != nil {
		t.Fatalf("preparing: %v", err)
	}
	if err := removeDevshardStorage(filepath.Join(escrowDir, "state.db"), base); err != nil {
		t.Fatalf("removing: %v", err)
	}
	if _, err := os.Stat(escrowDir); !os.IsNotExist(err) {
		t.Fatalf("the escrow directory survived: %v", err)
	}
}

// Test flow:
//  1. Call `removeDevshardStorage` with a blank path.
//  2. Assert it returns no error.
func TestRemoveDevshardStorageIgnoresAnEmptyPath(t *testing.T) {
	if err := removeDevshardStorage("  ", t.TempDir()); err != nil {
		t.Fatalf("got %v, want nil", err)
	}
}

// Test flow:
//  1. Seed an escrow's storage directory on disk.
//  2. Send a DELETE for that escrow through the admin route.
//  3. Assert the response is 200 and the store recorded exactly that escrow as deleted.
//  4. Assert the escrow's storage directory no longer exists.
func TestDeletingADevshardRemovesItsRowAndItsStorage(t *testing.T) {
	live := newHarness(t)
	escrowDir := filepath.Join(live.storageDir, "escrow-7")
	if err := os.MkdirAll(escrowDir, 0o700); err != nil {
		t.Fatalf("preparing: %v", err)
	}
	recorder := live.request(t, http.MethodDelete, "/v1/admin/devshards/7", "", adminHeaders())
	if recorder.Code != http.StatusOK {
		t.Fatalf("status: got %d (%s)", recorder.Code, recorder.Body.String())
	}
	if len(live.control.deleted) != 1 || live.control.deleted[0] != "7" {
		t.Fatalf("deleted rows: %v", live.control.deleted)
	}
	if _, err := os.Stat(escrowDir); !os.IsNotExist(err) {
		t.Fatalf("the escrow directory survived: %v", err)
	}
}

// Test flow:
//  1. Mark escrow "7" busy.
//  2. Send a DELETE for it through the admin route.
//  3. Assert the response is 409 and no row was deleted.
func TestDeletingABusyDevshardIsRefused(t *testing.T) {
	live := newHarness(t)
	live.escrows.busy["7"] = true
	recorder := live.request(t, http.MethodDelete, "/v1/admin/devshards/7", "", adminHeaders())
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409", recorder.Code)
	}
	if len(live.control.deleted) != 0 {
		t.Fatalf("a busy devshard's row was deleted: %v", live.control.deleted)
	}
}

// Test flow:
//  1. Make the control store's list call fail.
//  2. GET the admin devshards list.
//  3. Assert the response is 500.
func TestAStoreFailureOnTheOperatorRoutesIsA500(t *testing.T) {
	live := newHarness(t)
	live.control.listErr = errStoreUnavailable
	recorder := live.request(t, http.MethodGet, "/v1/admin/devshards", "", adminHeaders())
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", recorder.Code)
	}
}

// Test flow:
//  1. Mark escrow "7" busy.
//  2. POST a settle for it twice, once with no body and once with `force: true`.
//  3. Assert both attempts answer 409.
//  4. Assert no "settle" call was recorded, so a body-supplied force cannot buy past the busy check.
func TestSettlingABusyDevshardIsRefusedAndCannotBeForced(t *testing.T) {
	live := newHarness(t)
	live.escrows.busy["7"] = true
	for _, body := range []string{"", `{"force":true}`} {
		refused := live.request(t, http.MethodPost, "/v1/admin/devshards/7/settle", body, adminHeaders())
		if refused.Code != http.StatusConflict {
			t.Fatalf("settle with body %q: got %d (%s), want 409", body, refused.Code, refused.Body.String())
		}
	}
	if calls := live.operations.recordedCalls(); slices.Contains(calls, "settle") {
		t.Fatalf("a busy devshard reached the settle path: %v", calls)
	}
}

// Test flow:
//  1. Seed the control store with one devshard record.
//  2. GET the admin devshards list.
//  3. Assert the response is 200 and its body contains that escrow's ID.
func TestAnAdminListReflectsTheStore(t *testing.T) {
	live := newHarness(t)
	live.control.devshards = []store.DevshardRecord{{EscrowID: "42", Model: "kimi", Active: true}}
	recorder := live.request(t, http.MethodGet, "/v1/admin/devshards", "", adminHeaders())
	if recorder.Code != http.StatusOK {
		t.Fatalf("status: got %d", recorder.Code)
	}
	if got := recorder.Body.String(); !strings.Contains(got, `"id":"42"`) {
		t.Fatalf("body: %s", got)
	}
}

// Test flow:
//  1. Seed the control store with a devshard whose session is resident and one whose session is not.
//  2. GET the admin devshards list.
//  3. Assert the resident one carries its runtime with the session's nonce and the other carries none: an operator and the load harness tell a live escrow from a stored row by it.
func TestAnAdminListMarksTheEscrowsWithAResidentSession(t *testing.T) {
	live := newHarness(t)
	session, machine := newLiveSession(t)
	live.escrows.sessions[liveEscrowID] = registry.NewSessionHandle(session, machine)
	live.control.devshards = []store.DevshardRecord{{EscrowID: liveEscrowID, Model: "qwen", Active: true}, {EscrowID: "42", Model: "qwen"}}

	recorder := live.request(t, http.MethodGet, "/v1/admin/devshards", "", adminHeaders())

	var listed struct {
		Devshards []struct {
			ID      string `json:"id"`
			Runtime *struct {
				Nonce uint64 `json:"nonce"`
			} `json:"runtime"`
		} `json:"devshards"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &listed); err != nil || len(listed.Devshards) != 2 {
		t.Fatalf("list = %s (%v), want two devshards", recorder.Body.String(), err)
	}
	resident, stored := listed.Devshards[0], listed.Devshards[1]
	if resident.Runtime == nil || resident.Runtime.Nonce != session.Nonce() {
		t.Fatalf("resident devshard runtime = %+v, want the session's nonce %d", resident.Runtime, session.Nonce())
	}
	if stored.Runtime != nil {
		t.Fatalf("stored-only devshard runtime = %+v, want none", stored.Runtime)
	}
}

// Test flow:
//  1. Make the control store's list call fail and install a log capture.
//  2. GET the admin devshards list.
//  3. Assert an "admin request failed" line was logged at error with the 500 status, the route, and the store failure's text.
func TestAFailedAdminOperationIsLogged(t *testing.T) {
	entries := logcapture.Install(t)
	live := newHarness(t)
	live.control.listErr = errStoreUnavailable

	live.request(t, http.MethodGet, "/v1/admin/devshards", "", adminHeaders())

	entry, found := entries.Find("admin request failed")
	if !found {
		t.Fatalf("a 500 on an admin route left no log line: %+v", entries.All())
	}
	if entry.Level != "error" {
		t.Fatalf("level = %q, want error", entry.Level)
	}
	if got := logcapture.Field(entry, "status"); got != http.StatusInternalServerError {
		t.Fatalf("status field = %v, want 500", got)
	}
	if got := logcapture.Field(entry, "route"); got != "/v1/admin/devshards" {
		t.Fatalf("route field = %v", got)
	}
	if got, _ := logcapture.Field(entry, "error").(string); !strings.Contains(got, errStoreUnavailable.Error()) {
		t.Fatalf("error field = %q, want the store failure", got)
	}
}

// Test flow:
//  1. Install a log capture and send an admin GET request with no admin key.
//  2. Assert an "admin request refused" line was logged at warn with a 401 status.
func TestAnUnkeyedAdminCallIsLogged(t *testing.T) {
	entries := logcapture.Install(t)
	live := newHarness(t)

	live.request(t, http.MethodGet, "/v1/admin/devshards", "", nil)

	entry, found := entries.Find("admin request refused")
	if !found {
		t.Fatalf("an unkeyed admin call left no log line: %+v", entries.All())
	}
	if entry.Level != "warn" {
		t.Fatalf("level = %q, want warn", entry.Level)
	}
	if got := logcapture.Field(entry, "status"); got != http.StatusUnauthorized {
		t.Fatalf("status field = %v, want 401", got)
	}
}

// Test flow:
//  1. Install a log capture and send a properly keyed admin GET request.
//  2. Assert neither an "admin request failed" nor an "admin request refused" line was logged.
func TestASuccessfulAdminOperationIsNotLoggedAsAFailure(t *testing.T) {
	entries := logcapture.Install(t)
	live := newHarness(t)

	live.request(t, http.MethodGet, "/v1/admin/devshards", "", adminHeaders())

	if _, found := entries.Find("admin request failed"); found {
		t.Fatalf("a 200 was logged as a failure: %+v", entries.All())
	}
	if _, found := entries.Find("admin request refused"); found {
		t.Fatalf("a 200 was logged as a refusal: %+v", entries.All())
	}
}

func mustUnescape(t *testing.T, encoded string) string {
	t.Helper()
	decoded, err := url.PathUnescape(encoded)
	if err != nil {
		t.Fatalf("unescaping %q: %v", encoded, err)
	}
	return decoded
}

var errStoreUnavailable = errors.New("store unavailable")

// Test flow:
//  1. Make the escrow create fail because the wallet cannot pay for the amount and the fee.
//  2. POST an escrow create.
//  3. Assert it answers 402 with the wallet's shortfall, not a 502 that reads like a chain outage.
func TestAnEscrowCreateTheWalletCannotPayForAnswersPaymentRequired(t *testing.T) {
	live := newHarness(t)
	live.operations.err = fmt.Errorf("creating escrow for qwen/regular: %w", &chain.WalletUnderfundedError{Address: "gonka1wallet", Have: 5, Need: 11})

	response := live.request(t, http.MethodPost, "/v1/admin/escrows", `{"model_id":"qwen","amount":10,"private_key_env":"KEY"}`, adminHeaders())

	if response.Code != http.StatusPaymentRequired {
		t.Fatalf("status: got %d (%s), want 402", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "gonka1wallet holds 5") {
		t.Fatalf("body %s does not name the wallet's shortfall", response.Body.String())
	}
}

// Test flow:
//  1. Map a create refused below the model's floor, wrapped the way `createEscrow` returns it, through `statusForError`.
//  2. Assert it resolves to HTTP 400: the operator's amount is the bad input, not the chain.
func TestACreateBelowTheModelsFloorIsAnsweredAsABadRequest(t *testing.T) {
	refused := fmt.Errorf("creating escrow for model-a/regular: %w: amount 1000, floor 191000", escrow.ErrAmountBelowFloor)

	if got := statusForError(refused); got != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", got, http.StatusBadRequest)
	}
}
