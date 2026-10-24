package app

import (
	"common/vocabulary"

	"devshard/cmd/gateway/api"
	"devshard/cmd/gateway/chain"
	"devshard/cmd/gateway/filters"
	"devshard/cmd/gateway/tokenizer"
)

type pinnedPromptTokens struct {
	library *tokenizer.Library
}

func (p pinnedPromptTokens) Count(model string, body []byte) (api.TokenizedPrompt, bool) {
	counter := p.library.Counter(model)
	if counter == nil {
		return api.TokenizedPrompt{}, false
	}
	parts, err := filters.PromptText(body)
	if err != nil {
		return api.TokenizedPrompt{}, false
	}
	estimate, err := counter.EstimateTokens(parts)
	if err != nil {
		return api.TokenizedPrompt{}, false
	}
	return api.TokenizedPrompt{Tokens: estimate.Tokens, MinimumTokens: estimate.Minimum(), Sampled: estimate.Sampled}, true
}

func knownEpoch(snapshots phaseSnapshots) func() (uint64, bool) {
	return func() (uint64, bool) {
		epoch := snapshots.Snapshot().EpochIndex
		return epoch, epoch != chain.EpochIndexUnknown
	}
}

func promptTokens(modelSnapshots vocabulary.EpochGroupDataQuery, snapshots phaseSnapshots) api.PromptTokens {
	if modelSnapshots == nil {
		return nil
	}
	return pinnedPromptTokens{library: tokenizer.NewLibrary(vocabulary.ChainModelSource{Query: modelSnapshots}, knownEpoch(snapshots))}
}
