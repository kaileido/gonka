package peerload

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"devshard/cmd/gateway/chain"
)

type deadlineRegistry struct {
	*fakeRegistry
	mu    sync.Mutex
	reads []time.Duration
}

func (r *deadlineRegistry) MembersOf(ctx context.Context, addresses []string) ([]chain.RegistryMember, error) {
	deadline, bounded := ctx.Deadline()
	if !bounded {
		deadline = time.Now().Add(time.Hour)
	}
	r.mu.Lock()
	r.reads = append(r.reads, time.Until(deadline))
	r.mu.Unlock()
	return r.fakeRegistry.MembersOf(ctx, addresses)
}

type refusingRegistrar struct {
	mu      sync.Mutex
	refusal error
	writes  int
}

func (r *refusingRegistrar) RegisterInstance(context.Context, chain.RegistryRegistration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes++
	return r.refusal
}

func (r *refusingRegistrar) setRefusal(refusal error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refusal = refusal
}

func (r *refusingRegistrar) writeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writes
}

// Test flow:
//  1. Run one registry refresh that writes this instance's entry.
//  2. Assert every read carried a deadline of its own, no longer than the read timeout.
//  3. Assert the write carried one longer than the commit poll.
func TestRegistryCallsCarryADeadline(t *testing.T) {
	registry := &deadlineRegistry{fakeRegistry: newFakeRegistry()}
	var writeBudget time.Duration
	registrar := registrarFunc(func(ctx context.Context, _ chain.RegistryRegistration) error {
		if deadline, bounded := ctx.Deadline(); bounded {
			writeBudget = time.Until(deadline)
		}
		return nil
	})
	plugin := newTestPlugin(t, registry, registrar, newFakeClock().now)
	plugin.ObserveChain([]string{firstAddress}, testHeight)

	plugin.refreshRegistry(context.Background())

	if len(registry.reads) == 0 || slices.Max(registry.reads) > registryReadTimeout {
		t.Fatalf("read budgets = %v, want each at most %v", registry.reads, registryReadTimeout)
	}
	if writeBudget <= chain.DefaultPollTimeout {
		t.Fatalf("write budget = %v, want more than the %v commit poll", writeBudget, chain.DefaultPollTimeout)
	}
}

// Test flow:
//  1. Build a plugin from each endpoint the contract's validate_endpoint accepts, then from each it refuses.
//  2. Assert the first build and the others are refused as bad settings.
func TestAdvertiseURLFollowsTheContractGrammar(t *testing.T) {
	registry := newFakeRegistry()
	build := func(advertise string) error {
		_, err := New(Settings{Self: PeerID{Address: firstAddress, Instance: "self"}, AdvertiseURL: advertise, TransportKeyPEM: newKeyPEM(t)}, Deps{Registry: registry, Registrar: registry.registrar(firstAddress), Now: newFakeClock().now})
		return err
	}
	longestHost := strings.Repeat("a", endpointLimit-len("https://:8443"))
	for _, advertise := range []string{selfEndpoint, "https://[::1]:8443", "https://127.0.0.1:1", "https://GW-1.example:65535", "https://gw.example:00443", "https://" + longestHost + ":8443"} {
		if err := build(advertise); err != nil {
			t.Errorf("New(%q) = %v, the contract accepts it", advertise, err)
		}
	}
	for _, advertise := range []string{"https://gw.example", "https://gw.example:8443/", "https://gw.example:8443/peer", "https://u:p@gw.example:8443", "https://gw.example:8443?x=1", "https://:8443", "https://gw_x.example:8443", "https://gw.example:0", "http://gw.example:8443", "HTTPS://gw.example:8443", "https://gw.example:65536", "https://gw.example:+443", "https://gw.example:", "https://[]:8443", "https://[::1:8443", "https://[zz]:8443", "https://" + longestHost + "a:8443"} {
		if err := build(advertise); !errors.Is(err, ErrBadSettings) {
			t.Errorf("New(%q) = %v, want ErrBadSettings", advertise, err)
		}
	}
}

// Test flow:
//  1. Have the registrar refuse and refresh again right away; assert one write.
//  2. Move the clock through each wait, doubling from the refresh interval up to the cap; assert no write just before it ends and one write when it ends.
//  3. Let a write succeed, then refuse again; assert the write came at once and the wait is back to the refresh interval.
func TestRefusedWriteBacksOffUpToTheCapAndResetsOnSuccess(t *testing.T) {
	clock := newFakeClock()
	registrar := &refusingRegistrar{refusal: errors.New("not a writer")}
	plugin := newTestPlugin(t, newFakeRegistry(), registrar, clock.now)
	plugin.ObserveChain([]string{firstAddress}, testHeight)
	ctx := context.Background()

	plugin.refreshRegistry(ctx)
	plugin.refreshRegistry(ctx)
	if got := registrar.writeCount(); got != 1 {
		t.Fatalf("writes = %d after two refreshes, want 1", got)
	}

	want := 1
	for _, wait := range []time.Duration{300 * time.Second, 600 * time.Second, 1200 * time.Second, 2400 * time.Second, 4800 * time.Second, 9600 * time.Second, 19200 * time.Second, reregisterInterval, reregisterInterval} {
		clock.advance(wait - time.Second)
		plugin.refreshRegistry(ctx)
		if got := registrar.writeCount(); got != want {
			t.Fatalf("writes = %d one second before the %v wait ends, want %d", got, wait, want)
		}
		clock.advance(time.Second)
		plugin.refreshRegistry(ctx)
		want++
		if got := registrar.writeCount(); got != want {
			t.Fatalf("writes = %d when the %v wait ended, want %d", got, wait, want)
		}
	}

	registrar.setRefusal(nil)
	clock.advance(reregisterInterval)
	plugin.refreshRegistry(ctx)
	registrar.setRefusal(errors.New("not a writer"))
	plugin.refreshRegistry(ctx)
	if got := registrar.writeCount(); got != want+2 {
		t.Fatalf("writes = %d, want %d: a success clears the wait", got, want+2)
	}
	clock.advance(registryRefreshInterval)
	plugin.refreshRegistry(ctx)
	if got := registrar.writeCount(); got != want+3 {
		t.Fatalf("writes = %d, want %d: the wait restarts from the refresh interval", got, want+3)
	}
}

// Test flow:
//  1. Start the registry loop with the registry failing.
//  2. Assert the read is retried on the short interval.
//  3. Let the registry answer; assert the next read lands on the short interval and the one after waits for the refresh interval.
func TestFirstRegistryReadRetriesOnTheShortInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := newFakeRegistry()
		registry.setFailure(errors.New("node down"))
		plugin := newTestPlugin(t, registry, registry.registrar(secondAddress), time.Now)
		plugin.ObserveChain([]string{secondAddress}, testHeight)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go plugin.registryLoop(ctx)
		synctest.Wait()

		before := registry.callCount()
		time.Sleep(firstReadRetryInterval)
		synctest.Wait()
		if registry.callCount() == before {
			t.Fatal("no retry after a failed first read")
		}

		registry.setFailure(nil)
		time.Sleep(firstReadRetryInterval)
		synctest.Wait()
		answered := registry.callCount()
		time.Sleep(registryRefreshInterval - time.Second)
		synctest.Wait()
		if got := registry.callCount(); got != answered {
			t.Fatalf("reads = %d, want %d until the refresh interval ends", got, answered)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if got := registry.callCount(); got == answered {
			t.Fatal("no read when the refresh interval ended")
		}
		cancel()
		synctest.Wait()
	})
}

// Test flow:
//  1. Start the registry loop with an empty allowlist.
//  2. Change the allowlist without moving the clock.
//  3. Assert the loop read the registry at once.
func TestAllowlistChangeWakesTheRegistryLoopAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := newFakeRegistry()
		plugin := newTestPlugin(t, registry, registry.registrar(secondAddress), time.Now)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go plugin.registryLoop(ctx)
		synctest.Wait()

		plugin.ObserveChain([]string{secondAddress}, testHeight)
		synctest.Wait()

		if registry.callCount() == 0 {
			t.Fatal("the allowlist change did not wake the loop")
		}
		cancel()
		synctest.Wait()
	})
}

// Test flow:
//  1. Hold an entry of this instance whose key matches and whose endpoint differs.
//  2. Refresh.
//  3. Assert the entry is rewritten with the advertised endpoint.
func TestAnnounceRewritesAnEntryWithAnotherEndpoint(t *testing.T) {
	registry := newFakeRegistry()
	plugin := newTestPlugin(t, registry, registry.registrar(firstAddress), newFakeClock().now)
	registry.put(firstAddress, registryEntry("self", plugin.identity, "https://moved.example:8443"))
	plugin.ObserveChain([]string{firstAddress}, testHeight)

	plugin.refreshRegistry(context.Background())

	if got := registry.registrationCount(); got != 1 {
		t.Fatalf("registrations = %d, want 1", got)
	}
	if got := registry.registered[0].Endpoint; got != selfEndpoint {
		t.Fatalf("registered endpoint = %q, want %q", got, selfEndpoint)
	}
}

// Test flow:
//  1. Refresh once with no entry on chain, so the instance registers.
//  2. Assert its own entry is already readable, without waiting for the next refresh.
func TestAnnounceRereadsItsOwnEntryAtOnce(t *testing.T) {
	registry := newFakeRegistry()
	plugin := newTestPlugin(t, registry, registry.registrar(firstAddress), newFakeClock().now)
	plugin.ObserveChain([]string{firstAddress}, testHeight)

	plugin.refreshRegistry(context.Background())

	if _, found := plugin.members.Lookup(plugin.settings.Self); !found {
		t.Fatal("own entry unknown right after registering")
	}
}

// Test flow:
//  1. Allowlist this instance and an address whose read always fails.
//  2. Refresh.
//  3. Assert the instance still registers itself.
func TestOneBadAllowlistEntryDoesNotStopRegistration(t *testing.T) {
	registry := newFakeRegistry()
	plugin := newTestPlugin(t, poisonedRegistry{registry}, registry.registrar(firstAddress), newFakeClock().now)
	plugin.ObserveChain([]string{firstAddress, poisonedAddress}, testHeight)

	plugin.refreshRegistry(context.Background())

	if got := registry.registrationCount(); got != 1 {
		t.Fatalf("registrations = %d, want 1", got)
	}
}
