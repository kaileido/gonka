package tokenizer

import (
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// See README.md, "Estimating a long prompt".
const (
	exactBudgetBytes        = 64 << 10
	sampleWindows           = 8
	windowBytes             = exactBudgetBytes / sampleWindows
	sampledOvercountPercent = 7
	wholePercent            = 100
)

// See README.md, "The byte bound".
const (
	nfcMaxExpansion = 3
)

// MinimumTokens returns the fewest tokens prompt's parts hold, each counted on its own: exact up to exactBudgetBytes in all, the sampled estimate less its worst measured over-count beyond.
func (c *Counter) MinimumTokens(prompt []string) (uint64, error) {
	tokens, sampled, err := c.estimate(prompt)
	if err != nil || !sampled {
		return tokens, err
	}
	return tokens * wholePercent / (wholePercent + sampledOvercountPercent), nil
}

// MaximumTokens returns a count MinimumTokens never exceeds, read from prompt's bytes without tokenizing: every token covers at least one byte of the text it counts.
func (c *Counter) MaximumTokens(prompt []string) uint64 {
	total := 0
	for _, part := range prompt {
		total += len(part)
	}
	switch {
	case !c.nfc:
		return uint64(total)
	case total > exactBudgetBytes:
		for _, part := range prompt {
			if !isASCII(part) {
				return uint64(total) * nfcMaxExpansion
			}
		}
		return uint64(total)
	}
	normalized := 0
	for _, part := range prompt {
		normalized += c.normalizedBytes(part)
	}
	return uint64(normalized)
}

// normalizedBytes is text's length with each gap between its raw added tokens in NFC, as Count normalizes it; an added token is one token of at least one byte.
func (c *Counter) normalizedBytes(text string) int {
	gaps, _ := c.rawAddedTokens.split(text, nil)
	total := len(text)
	for _, gap := range gaps {
		total += len(norm.NFC.String(gap)) - len(gap)
	}
	return total
}

func isASCII(text string) bool {
	for index := range len(text) {
		if text[index] >= utf8.RuneSelf {
			return false
		}
	}
	return true
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
