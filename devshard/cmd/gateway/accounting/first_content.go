package accounting

import (
	"maps"
	"time"

	"devshard/cmd/gateway/engine"
)

// FirstContentTiming is the hosts' time to first content over the attempts that streamed any. See docs/accounting.md, "Time to first content".
type FirstContentTiming struct {
	FirstContentMillisecondsTotal uint64              `json:"first_content_ms_total"`
	FirstContentAttempts          uint64              `json:"first_content_attempts"`
	FirstContentBuckets           FirstContentBuckets `json:"first_content_buckets,omitempty"`
}

func (t *FirstContentTiming) add(other FirstContentTiming) {
	t.FirstContentMillisecondsTotal += other.FirstContentMillisecondsTotal
	t.FirstContentAttempts += other.FirstContentAttempts
	t.FirstContentBuckets = addInto(t.FirstContentBuckets, other.FirstContentBuckets)
}

func (t FirstContentTiming) clone() FirstContentTiming {
	t.FirstContentBuckets = maps.Clone(t.FirstContentBuckets)
	return t
}

func timingOf(delay time.Duration) FirstContentTiming {
	return FirstContentTiming{
		FirstContentMillisecondsTotal: uint64(delay.Milliseconds()),
		FirstContentAttempts:          1,
		FirstContentBuckets:           FirstContentBuckets{bucketOf(delay): 1},
	}
}

type firstContentReport struct {
	FirstContentTiming
	FirstContentP50Milliseconds uint64 `json:"first_content_p50_ms"`
	FirstContentP95Milliseconds uint64 `json:"first_content_p95_ms"`
}

func reportFirstContent(timing FirstContentTiming) firstContentReport {
	var report firstContentReport
	report.add(firstContentReport{FirstContentTiming: timing})
	return report
}

func (r *firstContentReport) add(other firstContentReport) {
	r.FirstContentTiming.add(other.FirstContentTiming)
	r.FirstContentP50Milliseconds = r.FirstContentBuckets.percentile(firstContentMedianPercent)
	r.FirstContentP95Milliseconds = r.FirstContentBuckets.percentile(firstContentTailPercent)
}

func (e *escrowLedger) addFirstContent(nonce uint64, record *nonceRecord, delay time.Duration) {
	if record.firstContentTimed || delay <= engine.NoFirstContent {
		return
	}
	record.firstContentTimed = true
	slotID := e.slotOf(nonce)
	timing := e.firstContent[slotID]
	timing.add(timingOf(delay))
	e.firstContent[slotID] = timing
}

func cloneFirstContent(timings map[uint32]FirstContentTiming) map[uint32]FirstContentTiming {
	cloned := make(map[uint32]FirstContentTiming, len(timings))
	for slotID, timing := range timings {
		cloned[slotID] = timing.clone()
	}
	return cloned
}
