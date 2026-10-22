package tokenizer

import "unicode/utf8"

// See README.md, "Estimating a long prompt".
const (
	exactBudgetBytes        = 64 << 10
	sampleWindows           = 8
	windowBytes             = exactBudgetBytes / sampleWindows
	windowPhaseStep         = windowBytes / 2
	sampledOvercountPercent = 7
	wholePercent            = 100
)

// MinimumTokens returns the fewest tokens prompt's parts hold, each counted on its own: exact up to exactBudgetBytes in all, the sampled estimate less its worst measured over-count beyond.
func (c *Counter) MinimumTokens(prompt []string) (uint64, error) {
	tokens, sampled, err := c.estimate(prompt)
	if err != nil || !sampled {
		return tokens, err
	}
	return tokens * wholePercent / (wholePercent + sampledOvercountPercent), nil
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
		start := window*span + (window*windowPhaseStep)%(span-windowBytes+1)
		windowTokens, windowBytesCounted, err := c.countRange(prompt, start, start+windowBytes)
		if err != nil {
			return 0, true, err
		}
		sampledTokens += windowTokens
		sampledBytes += windowBytesCounted
	}
	return (sampledTokens*uint64(total) + sampledBytes/2) / max(sampledBytes, 1), true, nil
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
