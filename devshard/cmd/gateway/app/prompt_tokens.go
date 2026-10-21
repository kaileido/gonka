package app

import (
	"common/vocabulary"

	"devshard/cmd/gateway/api"
	"devshard/cmd/gateway/tokenizer"
)

// promptTokens is nil without a chain: no pinned tokenizer can be named, so no prompt is checked against a context length.
func promptTokens(modelSnapshots vocabulary.EpochGroupDataQuery, snapshots phaseSnapshots) api.PromptTokens {
	if modelSnapshots == nil {
		return nil
	}
	return tokenizer.NewLibrary(vocabulary.ChainModelSource{Query: modelSnapshots}, func() uint64 { return snapshots.Snapshot().EpochIndex })
}
