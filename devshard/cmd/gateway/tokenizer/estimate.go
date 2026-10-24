package tokenizer

import (
	"unicode/utf8"
)

// See README.md, "Estimating a long prompt".
const (
	exactBudgetBytes        = 64 << 10
	sampleWindows           = 8
	windowBytes             = exactBudgetBytes / sampleWindows
	sampledOvercountPercent = 7
	wholePercent            = 100
)

// Estimate is one count of a prompt's tokens: exact, or past exactBudgetBytes the sampled estimate, Sampled then true.
type Estimate struct {
	Tokens  uint64
	Sampled bool
}

// EstimateTokens returns prompt's tokens, each part counted on its own: exact up to exactBudgetBytes in all, the sampled estimate beyond.
func (c *Counter) EstimateTokens(prompt []string) (Estimate, error) {
	tokens, sampled, err := c.estimate(prompt)
	return Estimate{Tokens: tokens, Sampled: sampled}, err
}

// Minimum returns the fewest tokens the estimated prompt holds: the exact count, or the sampled estimate less its worst measured over-count.
func (e Estimate) Minimum() uint64 {
	if !e.Sampled {
		return e.Tokens
	}
	return e.Tokens * wholePercent / (wholePercent + sampledOvercountPercent)
}

// MinimumTokens returns the fewest tokens prompt's parts hold, each counted on its own: EstimateTokens' Minimum.
func (c *Counter) MinimumTokens(prompt []string) (uint64, error) {
	estimate, err := c.EstimateTokens(prompt)
	return estimate.Minimum(), err
}

func (c *Counter) estimate(prompt []string) (tokens uint64, sampled bool, err error) {
	total := 0
	for _, part := range prompt {
		total += len(part)
	}
	if total <= exactBudgetBytes {
		tokens, _, err = c.countRange(prompt, 0, total)
		return tokens, false, err
	}
	span := total / sampleWindows
	var sampledTokens, sampledBytes uint64
	for window := range sampleWindows {
		start := windowStart(window, span)
		windowTokens, windowBytesCounted, err := c.countRange(prompt, start, start+windowBytes)
		if err != nil {
			return 0, true, err
		}
		sampledTokens += windowTokens
		sampledBytes += windowBytesCounted
	}
	return (sampledTokens*uint64(total) + sampledBytes/2) / max(sampledBytes, 1), true, nil
}

// windowStart places window inside its span-byte eighth: the first at the eighth's start, the last ending at its end, the others evenly between.
func windowStart(window, span int) int {
	return window*span + window*(span-windowBytes)/(sampleWindows-1)
}

func (c *Counter) countRange(prompt []string, start, end int) (tokens, bytesCounted uint64, err error) {
	offset := 0
	for _, part := range prompt {
		partStart, partEnd := max(start-offset, 0), min(end-offset, len(part))
		offset += len(part)
		partStart, partEnd = runeStartAtOrAfter(part, partStart), runeStartAtOrBefore(part, partEnd)
		if partStart >= partEnd {
			continue
		}
		segment := part[partStart:partEnd]
		segmentTokens, err := c.Count(segment)
		if err != nil {
			return 0, 0, err
		}
		tokens += uint64(segmentTokens)
		bytesCounted += uint64(len(segment))
	}
	return tokens, bytesCounted, nil
}

func runeStartAtOrAfter(text string, index int) int {
	for index < len(text) && !utf8.RuneStart(text[index]) {
		index++
	}
	return index
}

func runeStartAtOrBefore(text string, index int) int {
	for index > 0 && index < len(text) && !utf8.RuneStart(text[index]) {
		index--
	}
	return index
}
