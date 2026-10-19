package filters

import "devshard"

// Clamps applied to the thinking_token_budget a profile keeps.
const (
	thinkingBudgetAbsoluteMax     uint64 = 96_000
	thinkingBudgetContentHeadroom uint64 = 64
	thinkingBudgetDivisor         uint64 = 2
)

var allowedReasoningEffortValues = map[string]struct{}{
	"none": {}, "minimal": {}, "low": {}, "medium": {}, "high": {}, "xhigh": {}, "max": {},
}

// reasoningWrapper strips the wrapper, lifting .effort into an absent reasoning_effort unless enabled:false.
func reasoningWrapper() RuleFunc {
	return func(ctx RuleContext) error {
		raw, exists := ctx.Document.Get("reasoning")
		if !exists {
			return nil
		}
		ctx.Document.Delete("reasoning")
		wrapper, ok := raw.(map[string]any)
		if !ok {
			return nil
		}
		if enabled, ok := wrapper["enabled"].(bool); ok && !enabled {
			ctx.Document.Set("reasoning_effort", "none")
			return nil
		}
		effort, hasEffort := wrapper["effort"]
		if !hasEffort || ctx.Document.Has("reasoning_effort") {
			return nil
		}
		ctx.Document.Set("reasoning_effort", effort)
		return nil
	}
}

// reasoningEffortValidate rejects a value outside the enum; scoping to the routes that read it is a separate rule.
func reasoningEffortValidate() RuleFunc {
	return func(ctx RuleContext) error {
		raw, exists := ctx.Document.Get("reasoning_effort")
		if !exists {
			return nil
		}
		value, ok := raw.(string)
		if !ok {
			return Reject("reasoning_effort: invalid shape: must be a string")
		}
		if _, allowed := allowedReasoningEffortValues[value]; !allowed {
			return Reject("reasoning_effort: unsupported value: got %q", value)
		}
		return nil
	}
}

// enableThinking strips for ThinkingStrip profiles (no matching chat-template knob), else mirrors into kwargs.
func enableThinking() RuleFunc {
	return func(ctx RuleContext) error {
		if ctx.Profile.stripsThinking() {
			ctx.Document.Delete("enable_thinking")
			return nil
		}
		raw, exists := ctx.Document.Get("enable_thinking")
		if !exists {
			return nil
		}
		value, ok := raw.(bool)
		if !ok {
			return Reject("enable_thinking: must be a boolean: got %T", raw)
		}
		return mirrorFieldIntoKwargs(ctx.Document, "enable_thinking", value)
	}
}

// thinking strips, mirrors, or normalizes the type enum in place, per the profile's ThinkingDisposition.
func thinking() RuleFunc {
	return func(ctx RuleContext) error {
		if ctx.Profile.stripsThinking() {
			ctx.Document.Delete("thinking")
			return nil
		}
		raw, exists := ctx.Document.Get("thinking")
		if !exists {
			return nil
		}
		wrapper, ok := raw.(map[string]any)
		if !ok {
			return Reject("thinking: invalid wrapper shape: must be an object")
		}
		typeRaw, hasType := wrapper["type"]
		if !hasType {
			return Reject("thinking.type: must be \"enabled\", \"disabled\", \"adaptive\", or \"auto\": type is required")
		}
		typeValue, ok := typeRaw.(string)
		if !ok {
			return Reject("thinking.type: must be \"enabled\", \"disabled\", \"adaptive\", or \"auto\": type must be a string")
		}
		enabled, ok := resolveThinkingType(typeValue)
		if !ok {
			return Reject("thinking.type: must be \"enabled\", \"disabled\", \"adaptive\", or \"auto\": got %q", typeValue)
		}
		if ctx.Profile != nil && ctx.Profile.Thinking == ThinkingMirrorToKwargs {
			return mirrorFieldIntoKwargs(ctx.Document, "thinking", enabled)
		}
		if enabled {
			wrapper["type"] = "enabled"
		} else {
			wrapper["type"] = "disabled"
		}
		delete(wrapper, "display")
		return nil
	}
}

// resolveThinkingType maps thinking.type to its boolean intent; adaptive/auto both mean opt-in thinking.
func resolveThinkingType(typeValue string) (bool, bool) {
	switch typeValue {
	case "enabled", "adaptive", "auto":
		return true, true
	case "disabled":
		return false, true
	default:
		return false, false
	}
}

// getOrCreateChatTemplateKwargs returns the kwargs object, creating it when absent and rejecting a non-object.
func getOrCreateChatTemplateKwargs(document *Document) (map[string]any, error) {
	raw, exists := document.Get("chat_template_kwargs")
	if !exists {
		kwargs := map[string]any{}
		document.Set("chat_template_kwargs", kwargs)
		return kwargs, nil
	}
	kwargs, ok := raw.(map[string]any)
	if !ok {
		return nil, Reject("chat_template_kwargs: invalid wrapper shape: must be an object")
	}
	return kwargs, nil
}

// mirrorFieldIntoKwargs moves a top-level bool into kwargs[field], preserving a value already nested there.
func mirrorFieldIntoKwargs(document *Document, field string, value bool) error {
	kwargs, err := getOrCreateChatTemplateKwargs(document)
	if err != nil {
		return err
	}
	document.Delete(field)
	if _, exists := kwargs[field]; exists {
		return nil
	}
	kwargs[field] = value
	return nil
}

// silenceThinkingInKwargs overrules the caller: a request that cannot afford thinking cannot afford it in the template.
func silenceThinkingInKwargs(document *Document) error {
	kwargs, err := getOrCreateChatTemplateKwargs(document)
	if err != nil {
		return err
	}
	kwargs["thinking"] = false
	return nil
}

// forceThinkingOn overrules the caller's thinking switches for a ThinkingForceOn profile. See README.md, "Reasoning and thinking".
func forceThinkingOn() RuleFunc {
	return func(ctx RuleContext) error {
		if ctx.Profile == nil || ctx.Profile.Thinking != ThinkingForceOn {
			return nil
		}
		kwargs, err := getOrCreateChatTemplateKwargs(ctx.Document)
		if err != nil {
			return err
		}
		kwargs["enable_thinking"] = true
		return nil
	}
}

// thinkingTokenBudgetResolve clamps any budget so content keeps room. See README.md, "Reasoning and thinking".
func thinkingTokenBudgetResolve() RuleFunc {
	return func(ctx RuleContext) error {
		maxTokensRaw, exists := ctx.Document.Get("max_tokens")
		if !exists {
			return nil
		}
		maxTokens, ok := devshard.JSONNumericUint64(maxTokensRaw)
		if !ok || maxTokens == 0 {
			return nil
		}
		// Only a profile that declares the budget gets one invented: the V2 model runner rejects the field outright.
		if ctx.Profile != nil && ctx.Profile.ThinkingTokenBudget {
			if maxTokens < kimiThinkingBudgetForceZeroBelow {
				ctx.Document.Set("thinking_token_budget", uint64(0))
				// Overwrites rather than fills: the budget alone is a logits processor speculative decoding discards. See README.md, "Silencing Kimi's reasoning".
				return silenceThinkingInKwargs(ctx.Document)
			}
			if !ctx.Document.Has("thinking_token_budget") {
				ctx.Document.Set("thinking_token_budget", maxTokens/thinkingBudgetDivisor)
			}
		}
		budgetRaw, exists := ctx.Document.Get("thinking_token_budget")
		if !exists {
			return nil
		}
		budget, ok := devshard.JSONNumericUint64(budgetRaw)
		if !ok {
			return nil
		}
		if budget > thinkingBudgetAbsoluteMax {
			budget = thinkingBudgetAbsoluteMax
		}
		var headroomCap uint64
		if maxTokens > thinkingBudgetContentHeadroom {
			headroomCap = maxTokens - thinkingBudgetContentHeadroom
		}
		if budget > headroomCap {
			budget = headroomCap
		}
		ctx.Document.Set("thinking_token_budget", budget)
		return nil
	}
}

// thinkingTokenBudgetScope keeps the budget only for a profile that declares it: elsewhere the engine crashes on the field. See README.md, "Reasoning and thinking".
func thinkingTokenBudgetScope() RuleFunc {
	validate := requireUint()
	return func(ctx RuleContext) error {
		if ctx.Profile == nil || !ctx.Profile.ThinkingTokenBudget {
			ctx.Document.Delete(ctx.Param)
			return nil
		}
		return validate(ctx)
	}
}

// safetyIdentifier validates and keeps ctx.Param for AllowSafetyIdentifier profiles, and strips it otherwise.
func safetyIdentifier() RuleFunc {
	validate := requireString(safetyIdentifierMaxLen)
	return func(ctx RuleContext) error {
		if ctx.Profile == nil || !ctx.Profile.AllowSafetyIdentifier {
			ctx.Document.Delete(ctx.Param)
			return nil
		}
		return validate(ctx)
	}
}

// reasoningSplit passes the field through on the profile that can serve it and strips it elsewhere; an absent one stays absent.
func reasoningSplit() RuleFunc {
	return func(ctx RuleContext) error {
		if ctx.Profile == nil || !ctx.Profile.KeepReasoningSplit {
			ctx.Document.Delete(ctx.Param)
			return nil
		}
		raw, exists := ctx.Document.Get(ctx.Param)
		if !exists {
			return nil
		}
		if _, ok := raw.(bool); !ok {
			return Reject("%s: must be a boolean: got %T", ctx.Param, raw)
		}
		return nil
	}
}

// forceZeroPenalty overwrites ctx.Param to 0 for ForceZeroPenalties profiles, but only when already present.
func forceZeroPenalty() RuleFunc {
	return func(ctx RuleContext) error {
		if ctx.Profile == nil || !ctx.Profile.ForceZeroPenalties {
			return nil
		}
		if !ctx.Document.Has(ctx.Param) {
			return nil
		}
		ctx.Document.Set(ctx.Param, 0.0)
		return nil
	}
}
