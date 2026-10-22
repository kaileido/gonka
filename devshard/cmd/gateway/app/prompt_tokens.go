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

type countedPrompt struct {
	counter *tokenizer.Counter
	parts   []string
}

func (p pinnedPromptTokens) Prompt(model string, body []byte) (api.PromptCount, bool) {
	counter := p.library.Counter(model)
	if counter == nil {
		return nil, false
	}
	parts, err := filters.PromptText(body)
	if err != nil {
		return nil, false
	}
	return countedPrompt{counter: counter, parts: parts}, true
}

func (c countedPrompt) MaximumTokens() uint64 { return c.counter.MaximumTokens(c.parts) }

func (c countedPrompt) MinimumTokens() (uint64, bool) {
	tokens, err := c.counter.MinimumTokens(c.parts)
	return tokens, err == nil
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
