package peerload

import (
	"cmp"
	"context"
	"encoding/hex"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"devshard/cmd/gateway/chain"
)

// Registry is the read of the gateway-registry contract the membership needs.
type Registry interface {
	MembersOf(ctx context.Context, addresses []string) ([]chain.RegistryMember, error)
}

type peerInstance struct {
	ID            PeerID
	Endpoint      string
	Fingerprint   spkiFingerprint
	Protocol      uint16
	Topics        map[string]uint32
	UpdatedHeight uint64
}

// SupportsTopic reports whether the instance speaks this protocol and declares the topic at this version.
func (i peerInstance) SupportsTopic(name string, version uint32) bool {
	declared, ok := i.Topics[name]
	return i.Protocol == protocolVersion && ok && declared == version
}

type ownIdentity struct {
	ID           PeerID
	Fingerprint  spkiFingerprint
	AdvertiseURL string
}

type membership struct {
	self     ownIdentity
	registry Registry
	now      func() time.Time

	mu            sync.RWMutex
	allowed       map[string]struct{}
	height        int64
	instances     map[PeerID]peerInstance
	lastRefreshed map[string]time.Time
	hasRead       bool
}

func newMembership(self ownIdentity, registry Registry, now func() time.Time) *membership {
	return &membership{
		self:          self,
		registry:      registry,
		now:           now,
		allowed:       map[string]struct{}{},
		instances:     map[PeerID]peerInstance{},
		lastRefreshed: map[string]time.Time{},
	}
}

// ObserveChain takes the live allowlist and height, and reports whether the set of addresses changed.
func (m *membership) ObserveChain(allowed []string, height int64) bool {
	next := make(map[string]struct{}, len(allowed))
	for _, address := range allowed {
		next[address] = struct{}{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := len(next) != len(m.allowed)
	for address := range next {
		if _, known := m.allowed[address]; !known {
			changed = true
		}
	}
	m.allowed = next
	m.height = height
	for address := range m.lastRefreshed {
		if _, known := next[address]; !known {
			delete(m.lastRefreshed, address)
		}
	}
	return changed
}

// Height is the chain height of the last observation.
func (m *membership) Height() int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.height
}

// Allowed reports whether the address is in the live allowlist.
func (m *membership) Allowed(address string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.allowed[address]
	return ok
}

// Lookup returns the unexpired registry entry of an instance, this instance included.
func (m *membership) Lookup(id PeerID) (peerInstance, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	instance, ok := m.instances[id]
	if !ok || m.expiredLocked(instance) {
		return peerInstance{}, false
	}
	return instance, true
}

// Peers returns every instance to poll in a stable order: allowlisted, unexpired, never this instance.
func (m *membership) Peers() []peerInstance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	peers := make([]peerInstance, 0, len(m.instances))
	for id, instance := range m.instances {
		if _, allowed := m.allowed[id.Address]; allowed && !m.expiredLocked(instance) && !m.isSelf(instance) {
			peers = append(peers, instance)
		}
	}
	slices.SortFunc(peers, func(a, b peerInstance) int { return strings.Compare(a.ID.String(), b.ID.String()) })
	return peers
}

// HasRead reports whether a registry read has returned an answer for at least one allowlisted address.
func (m *membership) HasRead() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.hasRead
}

// Refresh rereads the registry for the whole allowlist, per address after a failed batch; an address that fails keeps its last good entries, and errPartialRead reports that others still answered.
func (m *membership) Refresh(ctx context.Context) error {
	m.mu.RLock()
	addresses := make([]string, 0, len(m.allowed))
	for address := range m.allowed {
		addresses = append(addresses, address)
	}
	m.mu.RUnlock()
	slices.Sort(addresses)

	budgeted, cancel := context.WithTimeout(ctx, registryRefreshBudget)
	defer cancel()
	read := make(map[PeerID]peerInstance)
	var failed []string
	var firstFailure error
	for batch := range slices.Chunk(addresses, registryBatchAddresses) {
		members, err := m.readMembers(budgeted, batch)
		if err == nil {
			mergeMembers(read, members, batch)
			continue
		}
		for _, address := range batch {
			members, err := m.readMembers(budgeted, []string{address})
			if err != nil {
				failed = append(failed, address)
				firstFailure = cmp.Or(firstFailure, err)
				continue
			}
			mergeMembers(read, members, []string{address})
		}
	}
	if len(addresses) > 0 && len(failed) == len(addresses) {
		return fmt.Errorf("read registry members: %w", firstFailure)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, instance := range m.instances {
		if slices.Contains(failed, id.Address) {
			read[id] = instance
		}
	}
	m.instances = read
	m.hasRead = m.hasRead || len(addresses) > 0
	if len(failed) > 0 {
		return fmt.Errorf("%w: %d of %d addresses: %w", errPartialRead, len(failed), len(addresses), firstFailure)
	}
	return nil
}

func (m *membership) readMembers(ctx context.Context, addresses []string) ([]chain.RegistryMember, error) {
	bounded, cancel := context.WithTimeout(ctx, registryReadTimeout)
	defer cancel()
	return m.registry.MembersOf(bounded, addresses)
}

// RefreshOne rereads one allowlisted address, at most once per 30 s; a call inside that span is a no-op.
func (m *membership) RefreshOne(ctx context.Context, address string) error {
	m.mu.Lock()
	_, allowed := m.allowed[address]
	last, seen := m.lastRefreshed[address]
	if !allowed || (seen && m.now().Sub(last) < refreshOnePerAddress) {
		m.mu.Unlock()
		return nil
	}
	m.lastRefreshed[address] = m.now()
	m.mu.Unlock()

	members, err := m.readMembers(ctx, []string{address})
	if err != nil {
		return fmt.Errorf("read registry member %s: %w", address, err)
	}
	read := make(map[PeerID]peerInstance)
	mergeMembers(read, members, []string{address})
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.instances {
		if id.Address == address {
			delete(m.instances, id)
		}
	}
	maps.Copy(m.instances, read)
	return nil
}

func (m *membership) expiredLocked(instance peerInstance) bool {
	return m.height-int64(instance.UpdatedHeight) >= instanceTTLBlocks
}

func (m *membership) isSelf(instance peerInstance) bool {
	return instance.ID == m.self.ID || instance.Fingerprint == m.self.Fingerprint || sameEndpoint(instance.Endpoint, m.self.AdvertiseURL)
}

func mergeMembers(into map[PeerID]peerInstance, members []chain.RegistryMember, asked []string) {
	for _, member := range members {
		if !slices.Contains(asked, member.Address) {
			continue
		}
		kept := 0
		for _, entry := range member.Instances {
			instance, ok := parseInstance(member.Address, entry)
			if !ok || kept == maxInstancesPerAddress {
				continue
			}
			into[instance.ID] = instance
			kept++
		}
	}
}

func parseInstance(address string, entry chain.RegistryInstance) (peerInstance, bool) {
	fingerprint, err := hex.DecodeString(entry.SPKISHA256)
	if err != nil || len(fingerprint) != len(spkiFingerprint{}) || !validInstanceID(entry.InstanceID) {
		return peerInstance{}, false
	}
	if !validEndpoint(entry.Endpoint) {
		return peerInstance{}, false
	}
	return peerInstance{
		ID:            PeerID{Address: address, Instance: entry.InstanceID},
		Endpoint:      entry.Endpoint,
		Fingerprint:   spkiFingerprint(fingerprint),
		Protocol:      entry.Protocol,
		Topics:        parseTopics(entry.Topics),
		UpdatedHeight: entry.UpdatedHeight,
	}, true
}

func parseTopics(declared []string) map[string]uint32 {
	topics := make(map[string]uint32, len(declared))
	for _, topic := range declared {
		name, versionText, separated := strings.Cut(topic, topicSplitter)
		if !separated {
			continue
		}
		if version, err := strconv.ParseUint(versionText, 10, 32); err == nil {
			topics[name] = uint32(version)
		}
	}
	return topics
}

func validEndpoint(endpoint string) bool {
	authority, isHTTPS := strings.CutPrefix(endpoint, httpsScheme+schemeSeparator)
	separator := strings.LastIndexByte(authority, portSeparator)
	if !isHTTPS || len(endpoint) > endpointLimit || separator < 0 {
		return false
	}
	port, err := strconv.ParseUint(authority[separator+1:], 10, 16)
	return err == nil && port != 0 && validHost(authority[:separator])
}

func validHost(host string) bool {
	if literal, bracketed := strings.CutPrefix(host, "["); bracketed {
		if address, closed := strings.CutSuffix(literal, "]"); closed {
			return address != "" && allBytes(address, func(b byte) bool { return isHexDigit(b) || b == portSeparator })
		}
	}
	return host != "" && allBytes(host, func(b byte) bool { return isLetter(b) || isDigit(b) || b == '.' || b == '-' })
}

func allBytes(text string, allowed func(byte) bool) bool {
	for index := range len(text) {
		if !allowed(text[index]) {
			return false
		}
	}
	return true
}

func isLetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func isHexDigit(b byte) bool { return isDigit(b) || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F') }

func sameEndpoint(first, second string) bool {
	firstURL, firstErr := url.Parse(first)
	secondURL, secondErr := url.Parse(second)
	if firstErr != nil || secondErr != nil {
		return false
	}
	return strings.EqualFold(firstURL.Scheme, secondURL.Scheme) && strings.EqualFold(firstURL.Host, secondURL.Host)
}
