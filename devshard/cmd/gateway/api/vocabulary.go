package api

// The strings the HTTP boundary puts on the wire and into the record: response headers a client reads,
// the verdict a finished request is logged under, and the trigger a capture file is named for. Declared
// here so a rename reaches the log, the tests and the docs through the compiler.

// Headers a client can read off any chat reply.
const (
	EscrowHeader    = "X-Devshard-ID"
	RequestIDHeader = "X-Request-Id"
)

// How a finished request was delivered. A stream commits 200 on its first byte, so this is the only
// place that tells a reply the client can retry from one it cannot.
const (
	deliveryServed                = "served"
	deliveryFailedMidStream       = "failed_mid_stream"
	deliveryFailedBeforeFirstByte = "failed_before_first_byte"
)

// The cache's own refusals, beside the ones filters can name from the body alone.
const (
	cacheRefusedTooLarge    = "too_large"
	cacheRefusedWriteFailed = "write_failed"
	cacheRefusedRaceFailed  = "race_failed"
	cacheRefusedNoEscrow    = "no_escrow"
)

// cacheResultStored is the cache-result label for a reply the cache kept.
const cacheResultStored = "stored"

// Why a request was captured. Capture has these two triggers and no others.
const (
	captureFilterRejected = "filter_rejected"
	captureAttemptsFailed = "all_attempts_failed"
)

// How a chain inference record reads in an admin answer, one name per protocol status.
const (
	statusNamePending     = "pending"
	statusNameStarted     = "started"
	statusNameFinished    = "finished"
	statusNameChallenged  = "challenged"
	statusNameValidated   = "validated"
	statusNameInvalidated = "invalidated"
	statusNameTimedOut    = "timed_out"
	statusNameUnknown     = "unknown"
)

// What phase an escrow session is in, as an admin answer names it.
const (
	phaseNameActive     = "active"
	phaseNameFinalizing = "finalizing"
	phaseNameSettlement = "settlement"
	phaseNameUnknown    = "unknown"
)
