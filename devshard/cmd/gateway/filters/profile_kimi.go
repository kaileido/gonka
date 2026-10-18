package filters

// Kimi's own constants: its thinking_token_budget resolution and its output token floor. The clamps every model shares live in rules_reasoning.go.
const (
	kimiThinkingBudgetForceZeroBelow uint64 = 256
	kimiOutputTokensFloor            uint64 = 16
)

// kimiProfile is Kimi's delta set. See README.md, "Model profiles".
var kimiProfile = &Profile{
	Models:                 []string{kimiModelID},
	ForceZeroPenalties:     true,
	RejectStructuredOutput: true,
	AllowSafetyIdentifier:  true,
	Thinking:               ThinkingMirrorToKwargs,
	ThinkingTokenBudget:    true,
	OutputTokensFloor:      kimiOutputTokensFloor,
}
