package filters

import (
	"sort"

	"common/completionapi"
)

type Stage int

const (
	// StagePreValidation runs before message hygiene and the token decode.
	StagePreValidation Stage = iota
	// StagePostLimits runs after output-token defaults/caps resolve.
	StagePostLimits
)

type RuleContext struct {
	Document *Document
	Param    string
	Profile  *Profile
}

// RuleFunc is one parameter rule: inspect/mutate ctx.Document, or reject.
type RuleFunc func(RuleContext) error

type StagedRule struct {
	Stage Stage
	Apply RuleFunc
}

type ParameterSpec struct {
	Name  string
	Rules []StagedRule
}

// Per-parameter bounds wired into parameterTable below.
const (
	minTemperature = 0.0
	maxTemperature = 2.0

	maxRepetitionPenalty = 2.0

	minPMin = 0.0
	minPMax = 1.0
	topPMax = 1.0

	topKMax = 262144

	modelMaxLen = 256

	penaltyMin = -2.0
	penaltyMax = 2.0

	userMaxLen             = 512
	safetyIdentifierMaxLen = 512

	messagesMaxEntries = 2048

	stopMaxEntries  = 16
	stopMaxEntryLen = 256

	badWordsMaxEntries  = 64
	badWordsMaxEntryLen = 128

	logprobsWidthCap = uint64(completionapi.ForcedTopLogprobs)

	logitBiasMinValue   = -100
	logitBiasMaxValue   = 100
	logitBiasMaxEntries = 1024

	metadataMaxKeys     = 16
	metadataMaxKeyLen   = 64
	metadataMaxValueLen = 512
)

var (
	// parameterTable is THE single registration point; knownParameterSet derives from it.
	parameterTable = []ParameterSpec{
		spec("model", StagePreValidation, validModelName(modelMaxLen)),
		{Name: "stream"},
		{Name: "max_tokens", Rules: []StagedRule{
			{Stage: StagePreValidation, Apply: liftNonPositiveOutputTokens()},
			{Stage: StagePreValidation, Apply: rejectNonPositiveOutputTokens()},
		}},
		{Name: "max_completion_tokens", Rules: []StagedRule{
			{Stage: StagePreValidation, Apply: liftNonPositiveOutputTokens()},
			{Stage: StagePreValidation, Apply: rejectNonPositiveOutputTokens()},
		}},
		spec("messages", StagePreValidation, validListLength(messagesMaxEntries, 0)),
		spec("seed", StagePreValidation, requireUint()),
		// Reservation budgets one max_tokens output; n choices can produce n times what it signed for.
		spec("n", StagePostLimits, replaceIfPresent(uint64(1))),
		spec("temperature", StagePostLimits, clampFloat(minTemperature, maxTemperature)),
		spec("repetition_penalty", StagePostLimits, rejectNonPositiveThenClamp(maxRepetitionPenalty)),
		spec("top_p", StagePostLimits, rejectNonPositiveThenClamp(topPMax)),
		spec("min_p", StagePostLimits, clampFloat(minPMin, minPMax)),
		spec("top_k", StagePostLimits, validTopK(topKMax)),
		{Name: "frequency_penalty", Rules: []StagedRule{
			{Stage: StagePostLimits, Apply: clampFloat(penaltyMin, penaltyMax)},
			{Stage: StagePostLimits, Apply: forceZeroPenalty()},
		}},
		{Name: "presence_penalty", Rules: []StagedRule{
			{Stage: StagePostLimits, Apply: clampFloat(penaltyMin, penaltyMax)},
			{Stage: StagePostLimits, Apply: forceZeroPenalty()},
		}},
		// The key check must run before the value/size rules below. See README.md, "Registration order is semantics".
		{Name: "logit_bias", Rules: []StagedRule{
			{Stage: StagePostLimits, Apply: requireTokenIDKeys()},
			{Stage: StagePostLimits, Apply: validFloatMap(logitBiasMinValue, logitBiasMaxValue, logitBiasMaxEntries)},
		}},
		spec("skip_special_tokens", StagePreValidation, requireBool()),
		spec("detokenize", StagePreValidation, requireBool()),
		spec("user", StagePreValidation, requireString(userMaxLen)),
		spec("logprobs", StagePreValidation, requireBool()),
		{Name: "top_logprobs", Rules: []StagedRule{
			{Stage: StagePreValidation, Apply: requireUint()},
			{Stage: StagePostLimits, Apply: capUint(logprobsWidthCap)},
		}},
		spec("return_token_ids", StagePreValidation, stripParameter()),
		spec("service_tier", StagePreValidation, stripParameter()),
		spec("store", StagePreValidation, stripParameter()),
		spec("provider", StagePreValidation, stripParameter()),
		spec("plugins", StagePreValidation, stripParameter()),
		spec("prompt_cache_key", StagePreValidation, stripParameter()),
		spec("cache_key", StagePreValidation, stripParameter()),
		spec("extra_headers", StagePreValidation, stripParameter()),
		spec("thinking_config", StagePreValidation, stripParameter()),
		spec("think", StagePreValidation, stripParameter()),
		{Name: "stop", Rules: []StagedRule{
			{Stage: StagePreValidation, Apply: requireStringElements()},
			{Stage: StagePreValidation, Apply: validListLength(stopMaxEntries, stopMaxEntryLen)},
		}},
		spec("stop_token_ids", StagePreValidation, stripParameter()),
		{Name: "bad_words", Rules: []StagedRule{
			{Stage: StagePreValidation, Apply: requireStringElements()},
			{Stage: StagePreValidation, Apply: dropBlankStringListElements()},
			{Stage: StagePreValidation, Apply: validListLength(badWordsMaxEntries, badWordsMaxEntryLen)},
		}},
		spec("metadata", StagePreValidation, validMetadata(metadataMaxKeys, metadataMaxKeyLen, metadataMaxValueLen)),
		spec("stream_options", StagePreValidation, validStreamOptions()),
		spec("min_tokens", StagePreValidation, requireUint()),
		// tools must precede tool_choice below. See README.md, "Registration order is semantics".
		spec("tools", StagePreValidation, validTools(SchemaBounds{
			MaxDepth:      toolsMaxDepth,
			MaxNodes:      toolsMaxNodes,
			MaxSizeBytes:  toolsMaxSizeBytes,
			MaxBranch:     toolsMaxBranch,
			MaxEnum:       toolsMaxEnum,
			MaxPatternLen: toolsMaxPatternLen,
		}, "auto")),
		spec("tool_choice", StagePreValidation, validToolChoice(toolChoiceMaxNameLen)),
		spec("parallel_tool_calls", StagePreValidation, parallelToolCalls()),
		spec("response_format", StagePreValidation, validResponseFormat(SchemaBounds{
			MaxDepth:      responseFormatMaxDepth,
			MaxNodes:      responseFormatMaxNodes,
			MaxSizeBytes:  responseFormatMaxSizeBytes,
			MaxBranch:     responseFormatMaxBranch,
			MaxEnum:       responseFormatMaxEnum,
			MaxPatternLen: responseFormatMaxPatternLen,
		}, responseFormatMaxNameLen)),
		spec("structured_outputs", StagePreValidation, validStructuredOutputs(structuredOutputsBounds{
			SchemaBounds: SchemaBounds{
				MaxDepth:      structuredOutputsMaxDepth,
				MaxNodes:      structuredOutputsMaxNodes,
				MaxSizeBytes:  structuredOutputsMaxSizeBytes,
				MaxBranch:     structuredOutputsMaxBranch,
				MaxEnum:       structuredOutputsMaxEnum,
				MaxPatternLen: structuredOutputsMaxPatternLen,
			},
			MaxChoiceEntries:    structuredOutputsMaxChoiceEntries,
			MaxChoiceEntryLen:   structuredOutputsMaxChoiceEntryLen,
			MaxGrammarLen:       structuredOutputsMaxGrammarLen,
			MaxGrammarNesting:   structuredOutputsMaxGrammarNesting,
			MaxStructuralTagLen: structuredOutputsMaxStructuralTagLen,
		})),
		spec("reasoning", StagePreValidation, reasoningWrapper()),
		spec("reasoning_effort", StagePreValidation, reasoningEffortValidate()),
		spec("enable_thinking", StagePreValidation, enableThinking()),
		// thinking must run before chat_template_kwargs below. See README.md, "Registration order is semantics".
		spec("thinking", StagePreValidation, thinking()),
		{Name: "thinking_token_budget", Rules: []StagedRule{
			{Stage: StagePreValidation, Apply: requireUint()},
			{Stage: StagePostLimits, Apply: thinkingTokenBudgetResolve()},
		}},
		spec("safety_identifier", StagePreValidation, safetyIdentifier()),
		spec("reasoning_split", StagePreValidation, reasoningSplit()),
		{Name: "chat_template_kwargs", Rules: []StagedRule{
			{Stage: StagePreValidation, Apply: validChatTemplateKwargs(ObjectBounds{
				MaxDepth:     chatTemplateKwargsMaxDepth,
				MaxNodes:     chatTemplateKwargsMaxNodes,
				MaxSizeBytes: chatTemplateKwargsMaxSizeBytes,
			})},
			{Stage: StagePostLimits, Apply: forceThinkingOn()},
		}},
	}

	// knownParameterSet is the whitelist, derived from parameterTable once at package init.
	knownParameterSet = buildKnownParameterSet()
)

func spec(name string, stage Stage, rule RuleFunc) ParameterSpec {
	return ParameterSpec{Name: name, Rules: []StagedRule{{Stage: stage, Apply: rule}}}
}

func buildKnownParameterSet() map[string]struct{} {
	known := make(map[string]struct{}, len(parameterTable))
	for _, spec := range parameterTable {
		known[spec.Name] = struct{}{}
	}
	return known
}

// unwrapExtraBody flattens an extra_body envelope before the whitelist runs; a top-level key wins on conflict.
func unwrapExtraBody(document *Document) {
	envelope, exists := document.Get("extra_body")
	if !exists {
		return
	}
	document.Delete("extra_body")
	inner, ok := envelope.(map[string]any)
	if !ok {
		return
	}
	for key, value := range inner {
		if key == "extra_body" || document.Has(key) {
			continue
		}
		document.Set(key, value)
	}
}

// rejectUnknownParameters is the whitelist gate; unknown keys are sorted so the report is deterministic.
func rejectUnknownParameters(document *Document) error {
	unknown := make([]string, 0)
	for key := range document.Raw() {
		if _, ok := knownParameterSet[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	name := unknown[0]
	if name == "" {
		return Reject("request body contains a field with an empty name")
	}
	return Reject("%s", unsupportedParameterMessage(name))
}
