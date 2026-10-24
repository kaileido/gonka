package accounting

import "time"

// FirstContentTiming sums the hosts' time to first content over the attempts that streamed any. See docs/accounting.md, "Time to first content".
type FirstContentTiming struct {
	FirstContentMilliseconds uint64 `json:"first_content_ms_total"`
	FirstContentAttempts     uint64 `json:"first_content_attempts"`
}

func (t *FirstContentTiming) add(other FirstContentTiming) {
	t.FirstContentMilliseconds += other.FirstContentMilliseconds
	t.FirstContentAttempts += other.FirstContentAttempts
}

func (e *escrowLedger) addFirstContent(nonce uint64, record *nonceRecord, delay time.Duration) {
	if record.firstContentTimed || delay <= 0 {
		return
	}
	record.firstContentTimed = true
	slotID := e.slotOf(nonce)
	timing := e.firstContent[slotID]
	timing.FirstContentMilliseconds += uint64(delay.Milliseconds())
	timing.FirstContentAttempts++
	e.firstContent[slotID] = timing
}
