package tokenizer

import (
	"strings"
	"testing"
)

func sentencesOf(size int) string {
	const sentence = "The gateway counts a prompt before it commits a nonce. "
	return strings.Repeat(sentence, size/len(sentence)+1)[:size]
}

// Test flow:
//  1. Build an ASCII prompt of exactly exactBudgetBytes and split it into parts.
//  2. Take MinimumTokens of it.
//  3. Assert it is the sum of each part's exact count.
func TestMinimumTokensIsExactUpToTheBudget(t *testing.T) {
	counter := loadTestdata(t, "isolated")
	text := sentencesOf(exactBudgetBytes)
	prompt := []string{text[:exactBudgetBytes/3], text[exactBudgetBytes/3:]}

	got, err := counter.MinimumTokens(prompt)
	if err != nil {
		t.Fatal(err)
	}

	want := 0
	for _, part := range prompt {
		counted, err := counter.Count(part)
		if err != nil {
			t.Fatal(err)
		}
		want += counted
	}
	if got != uint64(want) {
		t.Fatalf("MinimumTokens = %d, want the exact %d", got, want)
	}
}

// Test flow:
//  1. Build a prompt one byte past exactBudgetBytes, so it is sampled.
//  2. Take MinimumTokens of it, its sampled estimate and its exact count.
//  3. Assert MinimumTokens is the estimate less sampledOvercountPercent, and below the exact count.
func TestMinimumTokensPastTheBudgetIsTheSampledEstimateLessItsWorstOvercount(t *testing.T) {
	counter := loadTestdata(t, "isolated")
	prompt := []string{sentencesOf(exactBudgetBytes + 1)}

	got, err := counter.MinimumTokens(prompt)
	if err != nil {
		t.Fatal(err)
	}
	estimate, sampled, err := counter.estimate(prompt)
	if err != nil {
		t.Fatal(err)
	}
	exact, err := counter.Count(prompt[0])
	if err != nil {
		t.Fatal(err)
	}

	if !sampled || got != estimate*wholePercent/(wholePercent+sampledOvercountPercent) {
		t.Fatalf("MinimumTokens = %d (sampled %v), want the estimate %d less %d%%", got, sampled, estimate, sampledOvercountPercent)
	}
	if got >= uint64(exact) {
		t.Fatalf("MinimumTokens = %d, want below the exact %d", got, exact)
	}
}

// Test flow:
//  1. Build a prompt of about 1 MB that repeats the golden texts, as one part and as parts whose boundaries cut multi-byte runes across windows.
//  2. Estimate both and count the whole prompt exactly.
//  3. Assert each estimate is within 1% of the exact count.
func TestEstimateScalesSampledWindowsToTheWholePrompt(t *testing.T) {
	counter := loadTestdata(t, "isolated")
	text := strings.Repeat(strings.Join(goldenTexts, " "), 3_000)
	exact, err := counter.Count(text)
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for start := 0; start < len(text); start += 40_001 {
		parts = append(parts, text[start:min(start+40_001, len(text))])
	}

	for name, prompt := range map[string][]string{"one part": {text}, "many parts": parts} {
		got, sampled, err := counter.estimate(prompt)
		if err != nil {
			t.Fatal(err)
		}
		if drift := float64(got)/float64(exact) - 1; !sampled || drift > 0.01 || drift < -0.01 {
			t.Errorf("%s: estimate = %d (sampled %v) over %d bytes, exact %d (%.2f%%), want sampled within 1%%", name, got, sampled, len(text), exact, 100*drift)
		}
	}
}
