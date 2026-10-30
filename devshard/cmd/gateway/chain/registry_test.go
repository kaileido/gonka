package chain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	wasmtypes "github.com/CosmWasm/wasmd/x/wasm/types"
	sdktx "github.com/cosmos/cosmos-sdk/types/tx"
)

const fixedContract = "gonka1contract"

func fixedRegistration() RegistryRegistration {
	return RegistryRegistration{InstanceID: "instance-a", Endpoint: "https://a.example:8443", SPKISHA256: strings.Repeat("ab", 32), Protocol: 1, Topics: []string{"load@1"}}
}

func answerEveryBroadcast(transport *fakeTransport, result TxResult) {
	transport.onTx = func(int) {
		transport.mu.Lock()
		defer transport.mu.Unlock()
		transport.txs[txHashFromBytes(transport.broadcast[len(transport.broadcast)-1])] = result
	}
}

func executeMessageOf(t *testing.T, txBytes []byte) (string, *wasmtypes.MsgExecuteContract) {
	t.Helper()
	var raw sdktx.TxRaw
	if err := raw.Unmarshal(txBytes); err != nil {
		t.Fatalf("unmarshal tx: %v", err)
	}
	var body sdktx.TxBody
	if err := body.Unmarshal(raw.BodyBytes); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if len(body.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(body.Messages))
	}
	var message wasmtypes.MsgExecuteContract
	if err := message.Unmarshal(body.Messages[0].Value); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	if !body.Unordered {
		t.Fatal("tx is not unordered")
	}
	return body.Messages[0].TypeUrl, &message
}

// Test flow:
//  1. Decode a reply in the contract's tuple form, with a plain JSON height.
//  2. Assert every field arrives per address.
//  3. Assert a tuple of the wrong length is an error.
func TestDecodeRegistryMembersReadsTheContractsTupleForm(t *testing.T) {
	reply := `[["gonka1a",[{"instance_id":"a","endpoint":"https://a.example","spki_sha256":"abcd","protocol":1,"topics":["load@1"],"updated_height":123}]],["gonka1b",[]]]`

	members, err := decodeRegistryMembers([]byte(reply))
	if err != nil {
		t.Fatalf("decodeRegistryMembers() = %v", err)
	}

	if len(members) != 2 || members[0].Address != "gonka1a" || members[1].Address != "gonka1b" || len(members[1].Instances) != 0 {
		t.Fatalf("members = %+v", members)
	}
	instance := members[0].Instances[0]
	if instance.InstanceID != "a" || instance.Endpoint != "https://a.example" || instance.SPKISHA256 != "abcd" || instance.Protocol != 1 || instance.Topics[0] != "load@1" || instance.UpdatedHeight != 123 {
		t.Fatalf("instance = %+v", instance)
	}
	if _, err := decodeRegistryMembers([]byte(`[["gonka1a"]]`)); err == nil {
		t.Fatal("a one-element tuple decoded")
	}
}

// Test flow:
//  1. Marshal the members_of query.
//  2. Assert the JSON is the contract's snake-case enum form.
func TestMembersOfQueryIsTheContractsJSON(t *testing.T) {
	encoded, err := json.Marshal(membersOfQuery{MembersOf: membersOfArguments{Addresses: []string{"gonka1a"}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"members_of":{"addresses":["gonka1a"]}}`; string(encoded) != want {
		t.Fatalf("query = %s, want %s", encoded, want)
	}
}

// Test flow:
//  1. Register an instance through a client with a fake transport whose tx is committed.
//  2. Decode the tx it broadcast.
//  3. Assert an unordered MsgExecuteContract from the signer to the contract, with the contract's register_instance JSON.
func TestRegisterInstanceBroadcastsTheExecuteMessage(t *testing.T) {
	transport := newFakeTransport()
	answerEveryBroadcast(transport, TxResult{})
	client := newFakeTxClient(t, transport)
	signer := fixedSigner(t)

	if err := client.RegisterInstance(context.Background(), signer, fixedContract, fixedRegistration()); err != nil {
		t.Fatalf("RegisterInstance() = %v", err)
	}

	if len(transport.broadcast) != 1 {
		t.Fatalf("broadcasts = %d, want 1", len(transport.broadcast))
	}
	typeURL, message := executeMessageOf(t, transport.broadcast[0])
	if typeURL != executeContractMsgTypeURL || message.Sender != signer.Address() || message.Contract != fixedContract {
		t.Fatalf("type %s, message %+v", typeURL, message)
	}
	want := `{"register_instance":{"instance_id":"instance-a","endpoint":"https://a.example:8443","spki_sha256":"` + strings.Repeat("ab", 32) + `","protocol":1,"topics":["load@1"]}}`
	if string(message.Msg) != want {
		t.Fatalf("msg = %s, want %s", message.Msg, want)
	}
}

// Test flow:
//  1. Register with no signer.
//  2. Register with a tx the chain rejects.
//  3. Assert each returns an error.
func TestRegisterInstanceReportsARefusal(t *testing.T) {
	transport := newFakeTransport()
	client := newFakeTxClient(t, transport)
	if err := client.RegisterInstance(context.Background(), nil, fixedContract, fixedRegistration()); err == nil {
		t.Fatal("no signer: want an error")
	}

	answerEveryBroadcast(transport, TxResult{Code: 5, RawLog: "not a writer"})
	err := client.RegisterInstance(context.Background(), fixedSigner(t), fixedContract, fixedRegistration())
	if err == nil || !strings.Contains(err.Error(), "not a writer") || !strings.Contains(err.Error(), "awaiting registration commit for tx") {
		t.Fatalf("RegisterInstance() = %v, want the chain's refusal", err)
	}
}

// Test flow:
//  1. Build the tx with an empty chain id.
//  2. Assert it is refused before signing.
func TestBuildExecuteContractTxNeedsAChainID(t *testing.T) {
	if _, err := buildExecuteContractTx(fixedSigner(t), " ", 1, "ngonka", 0, 1, fixedContract, []byte("{}"), time.Unix(1, 0)); err == nil {
		t.Fatal("empty chain id: want an error")
	}
}

// Test flow:
//  1. Publish a snapshot from params carrying an allowlist.
//  2. Make the next params read fail.
//  3. Assert the allowlist is published, then kept from the last good read.
func TestPhaseObserverCarriesAllowedCreatorsAndKeepsThemOnAFailedRead(t *testing.T) {
	stub := newPhaseObserverStub()
	server := httptest.NewServer(stub.handler())
	defer server.Close()
	stub.setEpoch(http.StatusOK, observerEpochJSON(42, 4, EpochPhaseInference))
	stub.setParticipants(http.StatusOK, observerParticipantsJSON("gonka1abc", server.URL, 7))
	stub.setMaxNonceValue(1, true, nil)
	stub.setAllowedCreators([]string{"gonka1a", "gonka1b"})
	observer := newPoCPhaseObserver(t, server, stub)

	observer.refresh(context.Background())
	if got := observer.Snapshot().AllowedCreators; len(got) != 2 || got[0] != "gonka1a" {
		t.Fatalf("AllowedCreators = %v, want the chain's list", got)
	}

	stub.setMaxNonceValue(0, false, context.DeadlineExceeded)
	observer.refresh(context.Background())
	if got := observer.Snapshot().AllowedCreators; len(got) != 2 {
		t.Fatalf("AllowedCreators after a failed read = %v, want the last good list", got)
	}
}
