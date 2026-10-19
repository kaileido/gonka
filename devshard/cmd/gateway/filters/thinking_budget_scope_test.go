package filters

import "testing"

// Test flow:
//  1. Normalize a request carrying thinking_token_budget for each routed model whose profile does not declare the budget: a plain model and every registered profile but Kimi.
//  2. Assert the field is gone from each: the engines behind those models crash on it, and a client must not reach them with it.
func TestAThinkingTokenBudgetIsStrippedForAModelThatDoesNotDeclareIt(t *testing.T) {
	for _, model := range []string{"Qwen/Qwen3-235B-A22B-Instruct-2507-FP8", minimaxModelID, glm53FlashModelID, deepseekModelID} {
		t.Run(model, func(t *testing.T) {
			document := normalizedDocument(t, `{"messages":[{"role":"user","content":"x"}],"max_tokens":4096,"thinking_token_budget":100}`, model)
			requireAbsent(t, document, "thinking_token_budget")
		})
	}
}

// Test flow:
//  1. Normalize a request carrying thinking_token_budget for Kimi, whose profile declares it.
//  2. Assert the field is kept: Kimi resolves and clamps it rather than losing it.
func TestAThinkingTokenBudgetIsKeptForKimi(t *testing.T) {
	document := normalizedDocument(t, `{"messages":[{"role":"user","content":"x"}],"max_tokens":4096,"thinking_token_budget":100}`, kimiModelID)
	requireUintField(t, document, "thinking_token_budget", 100)
}
