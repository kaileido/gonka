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

func (p pinnedPromptTokens) MinimumPromptTokens(model string, body []byte) (uint64, bool) {
	counter := p.library.Counter(model)
	if counter == nil {
		return 0, false
	}
	prompt, err := filters.PromptText(body)
	if err != nil {
		return 0, false
	}
	tokens, err := counter.MinimumTokens(prompt)
	return tokens, err == nil
}

func promptTokens(modelSnapshots vocabulary.EpochGroupDataQuery, snapshots phaseSnapshots) api.PromptTokens {
	if modelSnapshots == nil {
		return nil
	}
	currentEpoch := func() (uint64, bool) {
		epoch := snapshots.Snapshot().EpochIndex
		return epoch, epoch != chain.EpochIndexUnknown
	}
	return pinnedPromptTokens{library: tokenizer.NewLibrary(vocabulary.ChainModelSource{Query: modelSnapshots}, currentEpoch)}
}
