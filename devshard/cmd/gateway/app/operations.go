package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"devshard/cmd/gateway/api"
	"devshard/cmd/gateway/chain"
	"devshard/cmd/gateway/config"
	"devshard/cmd/gateway/env"
	"devshard/cmd/gateway/escrow"
	"devshard/cmd/gateway/limits"
	"devshard/cmd/gateway/nonces"
	"devshard/cmd/gateway/registry"
	"devshard/cmd/gateway/store"
)

// suspiciousHosts is the operator's pin list, written to the store first. See ../README.md, "Admin operations".
type suspiciousHosts struct {
	pins suspiciousHostStore

	mu    sync.RWMutex
	hosts map[string]bool
}
type suspiciousHostStore interface {
	ListSuspiciousHosts(ctx context.Context) ([]string, error)
	AddSuspiciousHost(ctx context.Context, participantKey string) error
	RemoveSuspiciousHost(ctx context.Context, participantKey string) error
}

func newSuspiciousHosts(ctx context.Context, pins suspiciousHostStore) (*suspiciousHosts, error) {
	pinned, err := pins.ListSuspiciousHosts(ctx)
	if err != nil {
		return nil, err
	}
	hosts := make(map[string]bool, len(pinned))
	for _, participantKey := range pinned {
		hosts[participantKey] = true
	}
	return &suspiciousHosts{pins: pins, hosts: hosts}, nil
}

func (s *suspiciousHosts) Suspicious(participantKey string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hosts[participantKey]
}

func (s *suspiciousHosts) List() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	listed := make([]string, 0, len(s.hosts))
	for host := range s.hosts {
		listed = append(listed, host)
	}
	sort.Strings(listed)
	return listed
}

func (s *suspiciousHosts) Add(ctx context.Context, participantKey string) error {
	if err := s.pins.AddSuspiciousHost(ctx, participantKey); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hosts[participantKey] = true
	return nil
}

func (s *suspiciousHosts) Remove(ctx context.Context, participantKey string) error {
	if err := s.pins.RemoveSuspiciousHost(ctx, participantKey); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.hosts, participantKey)
	return nil
}

// escrowLifecycle is the chain-facing half of an operator action; *escrow.Manager satisfies it.
type escrowLifecycle interface {
	CreateEscrow(ctx context.Context, model escrow.ModelConfig) (chain.CreateEscrowResult, error)
	Settle(ctx context.Context, escrowID string, force bool) (chain.SettleEscrowResult, error)
}
type operations struct {
	values       env.Values
	config       *config.Holder
	store        *store.Store
	escrows      *registry.Registry
	manager      escrowLifecycle
	participants *limits.ParticipantLimiter
	storageDir   string
	nonces       *nonces.Recorder
}

// CreateEscrow takes the name of the variable holding the signing key, never the key. See operations.md, "What is exposed".
func (o *operations) CreateEscrow(ctx context.Context, request api.CreateEscrowRequest) (chain.CreateEscrowResult, error) {
	if strings.TrimSpace(request.PrivateKeyEnv) == "" {
		return chain.CreateEscrowResult{}, api.ErrPrivateKeyEnvRequired
	}
	result, err := o.manager.CreateEscrow(ctx, escrow.ModelConfig{
		ModelID:       request.Model,
		Amount:        request.Amount,
		PrivateKeyEnv: request.PrivateKeyEnv,
	})
	if err != nil {
		return chain.CreateEscrowResult{}, err
	}
	if !request.Activate {
		return result, o.Deactivate(ctx, escrowID(result.EscrowID))
	}
	return result, o.escrows.Add(ctx, escrowID(result.EscrowID), request.Model)
}

func (o *operations) AddDevshard(ctx context.Context, request api.AddDevshardRequest) error {
	return o.register(ctx, store.DevshardRecord{
		EscrowID:      request.EscrowID,
		PrivateKeyEnv: request.PrivateKeyEnv,
		Model:         request.Model,
		Active:        request.Activate,
	})
}

// ImportDevshard copies the storage, so the gateway owns the only handle to what it serves; every refusal but a park landing during the copy comes before it.
func (o *operations) ImportDevshard(ctx context.Context, request api.ImportDevshardRequest) error {
	record := store.DevshardRecord{
		EscrowID:      request.EscrowID,
		PrivateKeyEnv: request.PrivateKeyEnv,
		Model:         request.Model,
		Active:        request.Activate,
	}
	if err := refuseUnregistrable(record); err != nil {
		return err
	}
	if err := o.refuseParked(ctx, record.EscrowID); err != nil {
		return err
	}
	if err := o.refuseServedStorage(ctx, record.EscrowID); err != nil {
		return err
	}
	storagePath, err := escrowStorage(o.storageDir, request.EscrowID)
	if err != nil {
		return err
	}
	if err := copySessionStorage(request.SourcePath, storagePath); err != nil {
		return err
	}
	return o.save(ctx, record)
}

func (o *operations) register(ctx context.Context, record store.DevshardRecord) error {
	if err := refuseUnregistrable(record); err != nil {
		return err
	}
	return o.save(ctx, record)
}

func refuseUnregistrable(record store.DevshardRecord) error {
	if !isChainEscrowID(record.EscrowID) {
		return fmt.Errorf("%w: %q", api.ErrInvalidEscrowID, record.EscrowID)
	}
	_, err := env.PrivateKey(record.PrivateKeyEnv)
	return err
}

func (o *operations) save(ctx context.Context, record store.DevshardRecord) error {
	listing := o.escrows.Listing()
	defer listing.Close()
	written, err := o.store.UpsertDevshardUnlessParked(ctx, record)
	if err != nil {
		return err
	}
	if !written {
		return parkedError(record.EscrowID)
	}
	if !record.Active {
		return nil
	}
	return addServing(ctx, listing, record)
}

func (o *operations) refuseServedStorage(ctx context.Context, id string) error {
	existing, err := findDevshard(ctx, o.store, id)
	if errors.Is(err, escrow.ErrUnknownEscrow) {
		return nil
	}
	if err != nil {
		return err
	}
	if existing.Active || o.escrows.IsBusy(id) {
		return fmt.Errorf("%w: escrow %s is serving", api.ErrDevshardExists, id)
	}
	return nil
}

// refuseParked keeps an import from replacing the storage of a parked escrow before its write would refuse it.
func (o *operations) refuseParked(ctx context.Context, id string) error {
	existing, err := findDevshard(ctx, o.store, id)
	if errors.Is(err, escrow.ErrUnknownEscrow) {
		return nil
	}
	if err != nil {
		return err
	}
	return refuseParkedRecord(existing)
}

// A parked escrow's balance is already committed: serving spends nonces the settlement misses.
func refuseParkedRecord(record store.DevshardRecord) error {
	if record.SettlementPending || record.SettleTxHash != "" {
		return parkedError(record.EscrowID)
	}
	return nil
}

func parkedError(escrowID string) error {
	return fmt.Errorf("%w: escrow %s is parked for settlement", api.ErrDevshardNotActivatable, escrowID)
}

func (o *operations) Activate(ctx context.Context, id string) error {
	listing := o.escrows.Listing()
	defer listing.Close()
	activated, err := o.store.ActivateDevshardUnlessParked(ctx, id)
	if err != nil {
		return err
	}
	record, err := findDevshard(ctx, o.store, id)
	if err != nil {
		return err
	}
	if !activated {
		return parkedError(id)
	}
	return addServing(ctx, listing, record)
}

// Deactivate and Settle stop routing before the row changes. See operations.md, "What is exposed".
func (o *operations) Deactivate(ctx context.Context, id string) error {
	if err := o.escrows.Retire(id); err != nil {
		return err
	}
	return o.store.SetDevshardActive(ctx, id, false)
}

func (o *operations) Settle(ctx context.Context, id string, force bool) (chain.SettleEscrowResult, error) {
	if err := o.escrows.Retire(id); err != nil {
		return chain.SettleEscrowResult{}, err
	}
	return o.manager.Settle(ctx, id, force)
}

func (o *operations) Unquarantine(_ context.Context, participantKey string) error {
	if !o.participants.ClearQuarantine(participantKey) {
		return fmt.Errorf("%w: %s", api.ErrUnknownParticipant, participantKey)
	}
	return nil
}

func (o *operations) ResetAccountingEpoch(_ context.Context, epoch uint64) (int, error) {
	return o.nonces.ResetEpoch(epoch)
}

func (o *operations) Reconfigure(ctx context.Context, overrides config.Overrides) error {
	next, err := config.Build(o.values, overrides)
	if err != nil {
		return err
	}
	if err := o.store.SaveOverrides(ctx, overrides); err != nil {
		return err
	}
	o.config.Swap(next)
	return nil
}
func escrowID(created uint64) string { return fmt.Sprintf("%d", created) }

func isChainEscrowID(id string) bool {
	parsed, err := strconv.ParseUint(id, 10, 64)
	return err == nil && strconv.FormatUint(parsed, 10) == id
}
