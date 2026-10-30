package peerload

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"devshard/cmd/gateway/chain"
)

func newTestMembership(t *testing.T, registry *fakeRegistry, clock *fakeClock) (*membership, transportIdentity) {
	t.Helper()
	selfID := PeerID{Address: firstAddress, Instance: "self"}
	identity := newIdentity(t, selfID)
	return newMembership(ownIdentity{ID: selfID, Fingerprint: identity.Fingerprint, AdvertiseURL: "https://self.example:8443"}, registry, clock.now), identity
}

// Test flow:
//  1. Register this instance, a replica of its address, a stale entry under another id with its own key, an entry on its own advertise URL, and one other address.
//  2. Refresh the membership.
//  3. Assert the peers are the replica and the other address only.
func TestPeersExcludeThisInstanceByIdentityKeyAndURLButKeepReplicas(t *testing.T) {
	registry := newFakeRegistry()
	members, identity := newTestMembership(t, registry, newFakeClock())
	registry.put(firstAddress, registryEntry("self", identity, "https://self.example:8443"))
	registry.put(firstAddress, registryEntry("old-self", identity, "https://old.example:8443"))
	registry.put(firstAddress, registryEntry("same-url", newIdentity(t, PeerID{Address: firstAddress, Instance: "same-url"}), "https://SELF.example:8443"))
	registry.put(firstAddress, registryEntry("replica", newIdentity(t, PeerID{Address: firstAddress, Instance: "replica"}), "https://replica.example:8443"))
	registry.put(secondAddress, registryEntry("other", newIdentity(t, PeerID{Address: secondAddress, Instance: "other"}), "https://other.example:8443"))
	members.ObserveChain([]string{firstAddress, secondAddress}, testHeight)

	refreshed(t, members)

	want := []string{firstAddress + "/replica", secondAddress + "/other"}
	if got := instanceNames(members.Peers()); !slices.Equal(got, want) {
		t.Fatalf("Peers() = %v, want %v", got, want)
	}
}

// Test flow:
//  1. Register a peer at a known height.
//  2. Observe a height one block before its expiry, then the expiry height.
//  3. Assert the peer is kept, then gone from both Peers and Lookup, without a new read.
func TestPeersDropAnEntryAtItsExpiryHeight(t *testing.T) {
	registry := newFakeRegistry()
	members, _ := newTestMembership(t, registry, newFakeClock())
	id := PeerID{Address: secondAddress, Instance: "other"}
	registry.put(secondAddress, registryEntry("other", newIdentity(t, id), "https://other.example:8443"))
	allowed := []string{secondAddress}
	members.ObserveChain(allowed, testHeight)
	refreshed(t, members)

	members.ObserveChain(allowed, testHeight+instanceTTLBlocks-1)
	if got := len(members.Peers()); got != 1 {
		t.Fatalf("Peers() before expiry has %d entries, want 1", got)
	}

	members.ObserveChain(allowed, testHeight+instanceTTLBlocks)
	if got := len(members.Peers()); got != 0 {
		t.Fatalf("Peers() at expiry has %d entries, want 0", got)
	}
	if _, found := members.Lookup(id); found {
		t.Fatal("Lookup() found an expired entry")
	}
}

// Test flow:
//  1. Register a peer and refresh.
//  2. Observe an allowlist without its address, then an empty one.
//  3. Assert the change is reported, the peer is gone at once and an empty list admits nobody.
func TestAllowlistRemovalDropsThePeerAndAnEmptyListAdmitsNobody(t *testing.T) {
	registry := newFakeRegistry()
	members, _ := newTestMembership(t, registry, newFakeClock())
	registry.put(secondAddress, registryEntry("other", newIdentity(t, PeerID{Address: secondAddress, Instance: "other"}), "https://other.example:8443"))
	members.ObserveChain([]string{secondAddress}, testHeight)
	refreshed(t, members)

	if changed := members.ObserveChain([]string{thirdAddress}, testHeight); !changed {
		t.Fatal("ObserveChain() = false for a changed allowlist")
	}
	if got := len(members.Peers()); got != 0 {
		t.Fatalf("Peers() has %d entries after removal, want 0", got)
	}
	if changed := members.ObserveChain([]string{thirdAddress}, testHeight+1); changed {
		t.Fatal("ObserveChain() = true for the same allowlist")
	}
	members.ObserveChain(nil, testHeight)
	if members.Allowed(thirdAddress) {
		t.Fatal("Allowed() = true on an empty allowlist")
	}
}

// Test flow:
//  1. Read a good registry.
//  2. Make the next read fail and refresh.
//  3. Assert the error is returned and the last good read stays.
func TestRefreshFailureKeepsTheLastGoodRead(t *testing.T) {
	registry := newFakeRegistry()
	members, _ := newTestMembership(t, registry, newFakeClock())
	registry.put(secondAddress, registryEntry("other", newIdentity(t, PeerID{Address: secondAddress, Instance: "other"}), "https://other.example:8443"))
	members.ObserveChain([]string{secondAddress}, testHeight)
	refreshed(t, members)

	failure := errors.New("node down")
	registry.setFailure(failure)
	if err := members.Refresh(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("Refresh() = %v, want the read failure", err)
	}
	if got := len(members.Peers()); got != 1 {
		t.Fatalf("Peers() has %d entries, want the last good 1", got)
	}
}

// Test flow:
//  1. Observe 250 addresses.
//  2. Refresh.
//  3. Assert the reads go in batches of at most the contract's 100 addresses.
func TestRefreshReadsTheRegistryInContractSizedBatches(t *testing.T) {
	registry := newFakeRegistry()
	members, _ := newTestMembership(t, registry, newFakeClock())
	addresses := make([]string, 0, 250)
	for index := range 250 {
		addresses = append(addresses, fmt.Sprintf("gonka1address%d", index))
	}
	members.ObserveChain(addresses, testHeight)

	refreshed(t, members)

	if got := registry.callCount(); got != 3 {
		t.Fatalf("MembersOf calls = %d, want 3", got)
	}
	for _, batch := range registry.calls {
		if len(batch) > registryBatchAddresses {
			t.Fatalf("batch of %d addresses", len(batch))
		}
	}
}

// Test flow:
//  1. Register 20 instances, an unparsable fingerprint and a plain http endpoint under one address.
//  2. Refresh.
//  3. Assert the bad entries are skipped and at most 16 are kept.
func TestRefreshBoundsAnAddressAndSkipsBadEntries(t *testing.T) {
	registry := newFakeRegistry()
	members, _ := newTestMembership(t, registry, newFakeClock())
	identity := newIdentity(t, PeerID{Address: secondAddress, Instance: "base"})
	registry.put(secondAddress, chain.RegistryInstance{InstanceID: "bad-key", Endpoint: "https://a.example:8443", SPKISHA256: "zz", Protocol: protocolVersion})
	registry.put(secondAddress, registryEntry("plain", identity, "http://plain.example:8443"))
	for index := range 20 {
		registry.put(secondAddress, registryEntry(fmt.Sprintf("instance-%d", index), identity, fmt.Sprintf("https://instance%d.example:8443", index)))
	}
	members.ObserveChain([]string{secondAddress}, testHeight)

	refreshed(t, members)

	peers := members.Peers()
	if len(peers) != maxInstancesPerAddress {
		t.Fatalf("Peers() has %d entries, want %d", len(peers), maxInstancesPerAddress)
	}
	for _, peer := range peers {
		if peer.ID.Instance == "bad-key" || peer.ID.Instance == "plain" {
			t.Fatalf("Peers() kept the bad entry %s", peer.ID)
		}
	}
}

// Test flow:
//  1. Call RefreshOne for an allowlisted address twice inside the limit, then after it.
//  2. Call it for an address outside the allowlist.
//  3. Assert two registry reads happened, none for the outsider.
func TestRefreshOneIsRateLimitedPerAddressAndOnlyForAllowlistedAddresses(t *testing.T) {
	registry := newFakeRegistry()
	clock := newFakeClock()
	members, _ := newTestMembership(t, registry, clock)
	members.ObserveChain([]string{secondAddress}, testHeight)
	ctx := context.Background()

	for _, step := range []time.Duration{0, refreshOnePerAddress - time.Second, time.Second} {
		clock.advance(step)
		if err := members.RefreshOne(ctx, secondAddress); err != nil {
			t.Fatal(err)
		}
	}
	if err := members.RefreshOne(ctx, thirdAddress); err != nil {
		t.Fatal(err)
	}

	if got := registry.callCount(); got != 2 {
		t.Fatalf("MembersOf calls = %d, want 2", got)
	}
}

const poisonedAddress = "gonka1typo"

type poisonedRegistry struct{ *fakeRegistry }

func (r poisonedRegistry) MembersOf(ctx context.Context, addresses []string) ([]chain.RegistryMember, error) {
	if slices.Contains(addresses, poisonedAddress) {
		return nil, errors.New("decoding bech32 failed")
	}
	return r.fakeRegistry.MembersOf(ctx, addresses)
}

// Test flow:
//  1. Observe a well-formed peer beside an address whose read fails, in one batch.
//  2. Refresh.
//  3. Assert the peer is kept and the error says the read was partial.
func TestOneBadAllowlistEntryLosesOnlyThatAddress(t *testing.T) {
	fake := newFakeRegistry()
	fake.put(secondAddress, registryEntry("other", newIdentity(t, PeerID{Address: secondAddress, Instance: "other"}), "https://other.example:8443"))
	members := newMembership(ownIdentity{ID: PeerID{Address: firstAddress, Instance: "self"}, AdvertiseURL: selfEndpoint}, poisonedRegistry{fake}, newFakeClock().now)
	members.ObserveChain([]string{secondAddress, poisonedAddress}, testHeight)

	err := members.Refresh(context.Background())

	if !errors.Is(err, errPartialRead) {
		t.Fatalf("Refresh() = %v, want errPartialRead", err)
	}
	if got := instanceNames(members.Peers()); !slices.Equal(got, []string{secondAddress + "/other"}) {
		t.Fatalf("Peers() = %v, want the one well-formed peer", got)
	}
}

// Test flow:
//  1. Read two peers, then fail only the reads naming one of them.
//  2. Refresh.
//  3. Assert the failing address keeps its last good entry while the other follows the chain.
func TestFailingAddressKeepsItsLastGoodEntry(t *testing.T) {
	fake := newFakeRegistry()
	fake.put(secondAddress, registryEntry("other", newIdentity(t, PeerID{Address: secondAddress, Instance: "other"}), "https://other.example:8443"))
	fake.put(poisonedAddress, registryEntry("kept", newIdentity(t, PeerID{Address: poisonedAddress, Instance: "kept"}), "https://kept.example:8443"))
	switching := &switchingRegistry{Registry: fake}
	members := newMembership(ownIdentity{ID: PeerID{Address: firstAddress, Instance: "self"}, AdvertiseURL: selfEndpoint}, switching, newFakeClock().now)
	members.ObserveChain([]string{secondAddress, poisonedAddress}, testHeight)
	refreshed(t, members)

	switching.Registry = poisonedRegistry{fake}
	fake.remove(secondAddress)
	if err := members.Refresh(context.Background()); !errors.Is(err, errPartialRead) {
		t.Fatalf("Refresh() = %v, want errPartialRead", err)
	}

	if got := instanceNames(members.Peers()); !slices.Equal(got, []string{poisonedAddress + "/kept"}) {
		t.Fatalf("Peers() = %v, want only the failing address's last good entry", got)
	}
}

type switchingRegistry struct{ Registry }

// Test flow:
//  1. Hold an entry under this instance's own address and instance id, with another key and another endpoint.
//  2. Assert it is not a peer.
func TestPeersExcludeAnEntryUnderThisInstancesOwnName(t *testing.T) {
	registry := newFakeRegistry()
	members, _ := newTestMembership(t, registry, newFakeClock())
	registry.put(firstAddress, registryEntry("self", newIdentity(t, PeerID{Address: firstAddress, Instance: "self"}), "https://stale.example:8443"))
	members.ObserveChain([]string{firstAddress}, testHeight)
	refreshed(t, members)

	if got := members.Peers(); len(got) != 0 {
		t.Fatalf("Peers() = %v, want none", instanceNames(got))
	}
}

// Test flow:
//  1. Observe two addresses, then only one of them.
//  2. Assert the removal reads as a change.
func TestRemovingAnAddressIsAnAllowlistChange(t *testing.T) {
	members, _ := newTestMembership(t, newFakeRegistry(), newFakeClock())
	members.ObserveChain([]string{firstAddress, secondAddress}, testHeight)

	if changed := members.ObserveChain([]string{firstAddress}, testHeight); !changed {
		t.Fatal("ObserveChain() = false after an address left")
	}
}

// Test flow:
//  1. Read a peer's entry, then remove it from the chain.
//  2. Reread that one address past the rate limit.
//  3. Assert the entry is gone.
func TestRefreshOneDropsAnEntryGoneFromTheChain(t *testing.T) {
	registry := newFakeRegistry()
	clock := newFakeClock()
	members, _ := newTestMembership(t, registry, clock)
	id := PeerID{Address: secondAddress, Instance: "other"}
	registry.put(secondAddress, registryEntry("other", newIdentity(t, id), "https://other.example:8443"))
	members.ObserveChain([]string{secondAddress}, testHeight)
	refreshed(t, members)

	registry.remove(secondAddress)
	if err := members.RefreshOne(context.Background(), secondAddress); err != nil {
		t.Fatalf("RefreshOne() = %v", err)
	}

	if _, found := members.Lookup(id); found {
		t.Fatal("the entry stayed after it left the chain")
	}
}
