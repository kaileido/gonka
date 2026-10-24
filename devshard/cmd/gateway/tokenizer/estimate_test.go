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
//  1. Build a prompt of exactly exactBudgetBytes and one a byte past it.
//  2. Take EstimateTokens and MinimumTokens of each.
//  3. Assert the first is its exact count, not sampled, the second the unbiased sampled estimate, and both Minimums are what MinimumTokens returns.
func TestEstimateTokensIsTheUnbiasedCountMinimumTokensLowers(t *testing.T) {
	counter := loadTestdata(t, "isolated")
	for _, size := range []int{exactBudgetBytes, exactBudgetBytes + 1} {
		prompt := []string{sentencesOf(size)}

		estimate, err := counter.EstimateTokens(prompt)
		if err != nil {
			t.Fatal(err)
		}
		minimum, err := counter.MinimumTokens(prompt)
		if err != nil {
			t.Fatal(err)
		}
		tokens, sampled, err := counter.estimate(prompt)
		if err != nil {
			t.Fatal(err)
		}

		if estimate != (Estimate{Tokens: tokens, Sampled: sampled}) || sampled != (size > exactBudgetBytes) {
			t.Fatalf("%d bytes: EstimateTokens = %+v, want %d tokens, sampled %v", size, estimate, tokens, size > exactBudgetBytes)
		}
		if estimate.Minimum() != minimum || (sampled && minimum >= estimate.Tokens) {
			t.Fatalf("%d bytes: Minimum = %d, MinimumTokens = %d, estimate %d", size, estimate.Minimum(), minimum, estimate.Tokens)
		}
	}
}

// Test flow:
//  1. Build a 1 MiB prompt of sampleWindows eighths, each English in its first half and Chinese in its second.
//  2. Estimate it and count it exactly.
//  3. Assert the estimate is within 1% of the exact count, so the windows sample both halves of every eighth.
func TestEstimateSamplesTheWholeOfEachEighth(t *testing.T) {
	counter := loadTestdata(t, "isolated")
	const promptBytes = 1 << 20
	const half = promptBytes / sampleWindows / 2
	chinese := strings.Repeat(goldenTexts[3], half/len(goldenTexts[3])+1)
	chinese = chinese[:runeStartAtOrBefore(chinese, half)]
	var builder strings.Builder
	for range sampleWindows {
		builder.WriteString(sentencesOf(half))
		builder.WriteString(chinese)
	}
	text := builder.String()
	exact, err := counter.Count(text)
	if err != nil {
		t.Fatal(err)
	}

	got, sampled, err := counter.estimate([]string{text})
	if err != nil {
		t.Fatal(err)
	}

	if drift := float64(got)/float64(exact) - 1; !sampled || drift > 0.01 || drift < -0.01 {
		t.Errorf("estimate = %d (sampled %v) over %d bytes, exact %d (%.2f%%), want sampled within 1%%", got, sampled, len(text), exact, 100*drift)
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

// Test flow:
//  1. Take the span of an eighth for prompts from just past exactBudgetBytes to 4 MiB.
//  2. Place every window in its eighth.
//  3. Assert the first window starts its eighth, the last ends its eighth, and consecutive offsets inside the eighth differ by at most one byte from an even step.
func TestWindowsSpreadAcrossTheirEighths(t *testing.T) {
	for _, total := range []int{exactBudgetBytes + 1, 100 << 10, 1 << 20, 4<<20 + 7} {
		span := total / sampleWindows
		step := float64(span-windowBytes) / (sampleWindows - 1)
		for window := range sampleWindows {
			offset := windowStart(window, span) - window*span
			if want := float64(window) * step; float64(offset) > want || float64(offset) < want-1 {
				t.Errorf("total %d, window %d: offset %d inside its eighth, want %.1f", total, window, offset, want)
			}
		}
		if first := windowStart(0, span); first != 0 {
			t.Errorf("total %d: first window starts at %d, want 0", total, first)
		}
		if lastEnd := windowStart(sampleWindows-1, span) + windowBytes; lastEnd != sampleWindows*span {
			t.Errorf("total %d: last window ends at %d, want its eighth's end %d", total, lastEnd, sampleWindows*span)
		}
	}
}
