# `accounting` — the per-nonce ledger

Every nonce the gateway commits costs the escrow money whether or not anyone was served. This package answers, for each one, **where it went**.

## What it owns

- **`Book`** — the ledger. Facts go in (`RecordRace`, `RecordGhost`, `RecordTimeout`, `RecordAppliedTimeout`, `RecordInvalidVerdict`, `ObserveHostStats`, …), and each nonce lands in exactly one counter keyed by `CounterKey`: how it ended, why, and the timing flags that were true of it.
- **Findings** (`findings.go`) — the operator-facing verdicts derived from those counters: a ratio, a threshold, a severity. A finding is a claim about a host, so it fires only past `findingMinimumVolume` nonces.
- **A read API** (`http.go`) — epochs, participants, one participant, and the protocol event feed (`events.go`), which maps a chain-applied verdict back to the nonce and the client request that spent it.
- **Persistence** (`store.go`, `sqlstore.go`, `sqlschema.go`, `sqlsave.go`, `sqlload.go`) — a periodic snapshot so a restart does not lose the epoch.

## What it does not own

It records; it does not decide. Nothing here withholds a host from routing, changes what is dispatched, or talks to the chain. The facts arrive from [`nonces`](../nonces/), which is the only writer.

## Boundaries

- **The response schema is frozen.** `ParticipantRecord`, `SlotRecord` and `EpochSummary` are read by a tracker outside this repository. Adding a key is tolerable; renaming or removing one is a regression. Internal carriers between `slots()` and `absorb()` stay unexported rather than taking a JSON tag.
- **`SchemaVersion` is one number for two readers.** It is the report's `schema_version` and the value `sqlstore` checks on open; a mismatch drops the tables rather than migrating them, because the ledger is an epoch of observations, not a system of record. It is **9**; `devshard/accounting` emits `schema_version` 6 over a different shape, and the number is the only thing telling the two apart.
- **Aggregation happens on read, not on write.** Every total above the counters is derivable, so a stored total could only ever disagree with its own parts.
- **`in_flight` counts nonces the chain has yet to settle; `in_flight_requests` counts clients still waiting for an answer.** A nonce is in flight from its hand-off (`RecordAssigned`) until the chain finishes it, which for a race loser or a refused nonce is its timeout. "Finishes" means the escrow's log holds the Finish — the record reads `Finished`, `Challenged`, `Validated` or `Invalidated` on a sweep (`FinishApplied`) — not that the host sent one: a Finish waiting in the gateway's mempool for the next diff, pinned for an error-miss claim, or stuck keeps its nonce in flight, which is how a stuck one shows. A request is in flight from its first hand-off until its race reports, the moment the client has its answer, so a loser nonce outlives its request. The limiter's `devshard_gateway_inflight_requests` is the same client count taken from the HTTP path.

## How a nonce is classified

A nonce is filed under a **CounterKey** — one bucket per observed combination of slot, disposition and the dimensions around it. Only combinations that happened exist, and every dimension is bounded by its producer, so an escrow holds a few hundred buckets rather than a row per nonce. The per-nonce rows behind the buckets last only while a nonce can still move (see "Storage").

Three shapes are the same struct summed at different levels: one slot of one escrow, one host across all of them, and one epoch across all hosts. Each is the sum of the rows below it, which is why aggregation happens on read rather than in storage — every total is derivable, so a stored total could only ever disagree with its own parts.

Open requests are **unioned rather than summed** when rows are folded: one request spends several nonces, so slots of the same escrow hold it at once and summing would count the client twice.

Two classifications are worth naming because they look like something else:

- **Committed and never dispatched** is not a ghost — nothing decided to burn it — and not in flight, because the race that held it has reported. It is an unfinished refusal: no host ever saw it.
- **An empty terminal is itself a fact**: the nonce reached classification without the race ever reporting on it. It is named rather than left blank, which reads as missing data and drops the bucket from any grouping by terminal.

Seeing a nonce raises the assigned watermark too. The chain's latest is polled on a sweep while nonces arrive on every race, so without this the counters outrun the range they are measured against and the gap reports as a disagreement with the chain.

Re-opening an escrow keeps its counters and **pins the epoch at first sighting**: one that re-stamped itself on every sweep would drag its whole history into the current epoch. Epoch is part of the query key for the same reason — a rotation leaves escrows of adjacent epochs live at once, and summing those would report one epoch's work under another's index.

## Service nonces

A diff with no inference still spends a nonce and, while the session is active, a fee. The journal names what each one was composed for, and the ledger counts them per slot by purpose apart from the dispositions, so `unclassified` holds only the facts the ledger lost. Their cost is the count times the escrow's fee per nonce, which the sweep reads off the session's frozen config, less the finalize nonces, which the chain does not charge. Only the first finalize round names itself, so the ledger takes the finalize start from that round or from the chain's `FinalizeNonce`, whichever it sees first. See [`docs/accounting.md`](../docs/accounting.md), "Service nonces".

## The ledger's check on itself

`namesNoReason` counts the nonces whose cause the ledger could not name, matching the fallbacks the producers actually emit: an engine terminal with no string of its own, an attempt the race never classified, and a burn kind with no reason. `unreported` is **not** one of them — that names a race that reported nothing, which is a fact rather than a gap.

`timeoutOutcomeOf` folds the engine's action and reason into the single timeout outcome the ledger reports. `failed` alone says nothing a reader can act on; its reason is what tells a round that gathered too few votes from one that could not gather any.

`MoneyTotals` reads one escrow's money as the ledger last observed it: the reserved money of its Pending and Started records, the actual cost of its Challenged ones, and the sum of its slots' `HostStats.Cost`. These are the reserved, challenged and charged money of the last observation, which the scenario harness compares with the session and the chain (its `ledger match` invariant, see [`scenarios/README.md`](../scenarios/README.md)).

## Storage

The pragmas ride the connection string rather than a first statement, because the pool recreates connections and a recreated one would come back without them. One connection, because every write is a single transaction and `Store.writing` admits one at a time: two saves committing out of order would let the older copy of an escrow overwrite the newer one.

Nothing queries this database except the ledger's own load at start-up — every question an operator asks is answered from memory. The tables therefore mirror the in-memory shape one for one. The book marks every escrow a mutation touches, and a save rewrites only the escrows marked since the last one: it deletes their rows by `escrow_id` and inserts them again, and an escrow the book dropped (pruned, or cleared by an epoch reset) is only deleted. A save that fails marks them again, and a restore marks everything, so the first save after a start writes the ledger whole. A load that fails leaves the book empty and marks the stored ledger itself for replacement: the first save empties every table before it writes, as a ledger that started empty must, rather than leaving the unreadable rows to come back on the next start. Each insert is prepared once per save, because a save writes hundreds of rows per escrow through the same prepared statements. The cost of a save follows the escrows that changed, not the size of the ledger. Each save is one transaction, which is what makes a half-written ledger impossible: a reader sees the previous contents until the commit, and a crash leaves them.

A column a table gains after it first shipped is listed in `addedColumns`, and `OpenStore` adds each one a stored table lacks, so a ledger written before it keeps its rows rather than being discarded with a new `SchemaVersion`; the tokenized money columns read zero there, and its nonces count as byte estimates. A new table needs no entry: the schema creates it when it is missing.

`SlotActivity` is the gateway's own side of the counts `HostStats` holds the chain's side of. Restoring one without the other reports every restart as a disagreement between the two.

A slot's money is two parts that never overlap. A nonce the chain still holds keeps its own cost, read again on every sweep and saved per nonce, because its status can still move. A nonce the chain has sealed away is folded into the slot's saved money on the first sweep that no longer sees it, and its cost is dropped, because nothing can change it any more. Saving the live costs rather than a total is what lets a restart neither lose a nonce sealed while the gateway was down nor count a live one twice, once from the save and once from the chain; folding the sealed ones keeps the live map as small as the chain's own window.

Only nonces whose disposition can still move are written down. A burned or finished one cannot — no later fact lifts it — while an unfinished one can, because the protocol may finish it after the race gave up, and one still awaiting its timeout has no disposition yet.

Memory keeps the same rule once the chain seals a nonce away: the sweep that folds its cost drops its row too unless its disposition can still move (`forgetSealed`). Not earlier: until the seal a finish can still meet its race report, which turns `finished_usage_unknown` into used or unused, and the money fold reads whether the nonce was burned. A sealed record has left the chain's live set, so no finish, timeout or verdict follows it, but its race can still report: the report waits for every sibling attempt, which can stream past the seal grace. So a finish counted before its race reported keeps its row, marked sealed, and the report drops it (`nonceRecord.raced`, `nonceRecord.sealed`); dropped earlier, the report would recreate the row and the sealed close would count the nonce a second time. Such a row is counted and never saved, so one whose race never reports goes with a restart or with its escrow's ledger (`PruneBefore`, `ResetEpoch`), and a row restored after a restart counts as raced, because its race ended with the process that ran it. A fact that still arrives for a dropped nonce is taken as it is after a restart, which holds the same rows.

## Read next

- [`docs/accounting.md`](../docs/accounting.md) — every finding, its threshold, and what an operator should do about it.
