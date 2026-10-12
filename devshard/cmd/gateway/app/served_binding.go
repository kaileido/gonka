package app

import (
	"devshard/cmd/gateway/metrics"
	"devshard/user"
)

type servedStreamHosts interface {
	RecordServedMismatch(participant, model string)
}

type servedStreamNarrator interface {
	HostServedUnsigned(escrowID, model, participant string, nonce uint64)
}

func servedBindingStrikes(hosts servedStreamHosts, narrator servedStreamNarrator, recorder *metrics.ServedBindingRecorder) func(escrowID, model, participant string, nonce uint64, verdict user.ServedBinding) {
	return func(escrowID, model, participant string, nonce uint64, verdict user.ServedBinding) {
		recorder.Observed(model, string(verdict))
		if verdict != user.ServedBindingMismatch {
			return
		}
		narrator.HostServedUnsigned(escrowID, model, participant, nonce)
		hosts.RecordServedMismatch(participant, model)
	}
}
