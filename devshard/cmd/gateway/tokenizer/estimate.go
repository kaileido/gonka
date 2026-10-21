package tokenizer

import "unicode/utf8"

// A prompt up to exactBudgetBytes is counted whole; a longer one is counted on sampleWindows windows of that budget. See README.md, "Estimating a long prompt".
const (
	exactBudgetBytes = 64 << 10
	sampleWindows    = 8
	windowBytes      = exactBudgetBytes / sampleWindows
)

// Estimate returns the token count of prompt's parts, each counted on its own: exact up to exactBudgetBytes in all, scaled from spread windows beyond.
func (c *Counter) Estimate(prompt []string) (uint64, error) {
	total := 0
	for _, part := range prompt {
		total += len(part)
	}
	if total <= exactBudgetBytes {
		tokens := 0
		for _, part := range prompt {
			counted, err := c.Count(part)
			if err != nil {
				return 0, err
			}
			tokens += counted
		}
		return uint64(tokens), nil
	}
	span := total / sampleWindows
	var tokens, sampled uint64
	for window := range sampleWindows {
		// Each window starts at its own phase within its span, so a prompt that alternates every few kilobytes is not sampled on one side only.
		start := window*span + (window*windowBytes/2)%(span-windowBytes+1)
		windowTokens, windowSampled, err := c.countRange(prompt, start, start+windowBytes)
		if err != nil {
			return 0, err
		}
		tokens += windowTokens
		sampled += windowSampled
	}
	if sampled == 0 {
		return 0, nil
	}
	return (tokens*uint64(total) + sampled/2) / sampled, nil
}

// countRange counts the bytes [start, end) of prompt's concatenation, each part's share on its own, cut at rune boundaries.
func (c *Counter) countRange(prompt []string, start, end int) (tokens, sampled uint64, err error) {
	offset := 0
	for _, part := range prompt {
		partStart, partEnd := max(start-offset, 0), min(end-offset, len(part))
		offset += len(part)
		partStart, partEnd = runeStartAtOrAfter(part, partStart), runeStartAtOrBefore(part, partEnd)
		if partStart >= partEnd {
			continue
		}
		segment := part[partStart:partEnd]
		counted, err := c.Count(segment)
		if err != nil {
			return 0, 0, err
		}
		tokens += uint64(counted)
		sampled += uint64(len(segment))
	}
	return tokens, sampled, nil
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
