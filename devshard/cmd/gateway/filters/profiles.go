package filters

import "slices"

// ThinkingDisposition is the closed set of ways a profile handles the thinking/enable_thinking fields. See README.md, "Model profiles".
type ThinkingDisposition int

const (
	ThinkingNormalizeInPlace ThinkingDisposition = iota
	ThinkingMirrorToKwargs
	ThinkingStrip
	ThinkingForceOn
)

// Profile captures one routed model's deltas from the default pipeline; a nil *Profile is the default. See README.md, "Model profiles".
type Profile struct {
	Models []string

	ForceZeroPenalties     bool
	RejectStructuredOutput bool
	AllowSafetyIdentifier  bool
	Thinking               ThinkingDisposition
	KeepReasoningSplit     bool
	ThinkingTokenBudget    bool

	OutputTokensFloor uint64
}

// Exact routed-model identifiers the parameter table dispatches on, and the floor of a profile that leaves a small output budget as the client sent it.
const (
	kimiModelID       = "moonshotai/Kimi-K2.6"
	minimaxModelID    = "MiniMaxAI/MiniMax-M2.7"
	deepseekModelID   = "deepseek-ai/DeepSeek-V4-Flash-0731"
	glm53FlashModelID = "zai-org/GLM-5.3-Flash"

	outputTokensFloorNone uint64 = 0
)

var registeredProfiles = []*Profile{kimiProfile, minimaxProfile, deepseekProfile, glm53FlashProfile}

func ProfileFor(routedModel string) *Profile {
	for _, profile := range registeredProfiles {
		if slices.Contains(profile.Models, routedModel) {
			return profile
		}
	}
	return nil
}

// stripsThinking is nil-safe: it reports a profile with no matching chat-template knob for thinking. See README.md, "Reasoning and thinking".
func (profile *Profile) stripsThinking() bool {
	return profile != nil && profile.Thinking == ThinkingStrip
}
