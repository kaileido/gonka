package escrow

import (
	"devshard/cmd/gateway/chain"
	"devshard/cmd/gateway/store"
)

func effectiveEpoch(snapshot chain.PhaseSnapshot) uint64 {
	if snapshot.EffectiveEpochIndex > 0 {
		return snapshot.EffectiveEpochIndex
	}
	return snapshot.EpochIndex
}

// ownEpochOver compares the effective epoch's lower bound with the chain epoch's upper bound, the label while unresolved.
func ownEpochOver(record store.DevshardRecord, snapshot chain.PhaseSnapshot) bool {
	chainEpoch := record.ChainEpoch
	if chainEpoch == 0 && record.RotationEpoch > 0 {
		chainEpoch = uint64(record.RotationEpoch)
	}
	return chainEpoch > 0 && effectiveEpochFloor(snapshot) > chainEpoch
}

func effectiveEpochFloor(snapshot chain.PhaseSnapshot) uint64 {
	if snapshot.EffectiveEpochIndex == 0 && snapshot.EpochIndex > 0 {
		return snapshot.EpochIndex - 1
	}
	return snapshot.EffectiveEpochIndex
}

func (m *Manager) prunableOnChain(record store.DevshardRecord) bool {
	if record.ChainEpoch == 0 || m.snapshots == nil {
		return false
	}
	snapshot := m.snapshots.Snapshot()
	snapshot.EffectiveEpochIndex = effectiveEpochFloor(snapshot)
	return snapshot.EffectiveEpochIndex > 0 && deadlineAt(record, snapshot, 0).passed
}

func bridgeLabel(snapshot chain.PhaseSnapshot) uint64 {
	return effectiveEpoch(snapshot) + 1
}
