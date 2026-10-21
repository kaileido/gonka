package tokenizer

import (
	"strings"
	"testing"
)

// Test flow:
//  1. Split a prompt under exactBudgetBytes into parts.
//  2. Estimate it.
//  3. Assert the estimate is the sum of each part's exact count.
func TestEstimateIsExactWithinTheBudget(t *testing.T) {
	counter := loadTestdata(t, "isolated")
	prompt := goldenTexts

	got, err := counter.Estimate(prompt)
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
		t.Fatalf("Estimate = %d, want the exact %d", got, want)
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
		got, err := counter.Estimate(prompt)
		if err != nil {
			t.Fatal(err)
		}
		if drift := float64(got)/float64(exact) - 1; drift > 0.01 || drift < -0.01 {
			t.Errorf("%s: Estimate = %d over %d bytes, exact %d (%.2f%%), want within 1%%", name, got, len(text), exact, 100*drift)
		}
	}
}
