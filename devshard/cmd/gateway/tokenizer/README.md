# `tokenizer` — a prompt's tokens, before anything is sent

Counts a chat prompt's tokens with the tokenizer the chain pins for the model, so the boundary can refuse a prompt the model's context cannot hold before a nonce exists ([`docs/request.md`](../docs/request.md), "Step 6 in detail").

## What it owns

- `bpe.go` — `Load` reads a Hugging Face `tokenizer.json` into a `Counter`; `Counter.Count` is its exact token count of a text, without the special tokens a chat template adds.
- `merge.go` — the merge loop: lowest rank first, leftmost on a tie, as Hugging Face BPE applies merges.
- `estimate.go` — `Counter.Estimate`: exact up to a budget, sampled beyond it.
- `library.go` — `Library`: the counter for each model, keyed by the revision the chain pins, loaded in the background.

## Where the tokenizer comes from

The epoch's model snapshot pins every model to a Hugging Face repo and commit (`hf_repo`, `hf_commit`), the revision hosts serve and validators replay. `Library` reads that pin through `vocabulary.ChainModelSource`, the same chain query the stop-token vocabulary reads (`app/stop_token_vocabulary.go`), and fetches `tokenizer.json` at that commit. A model id never selects a tokenizer by name, so a new model is counted the epoch governance registers it, and a gateway is never counting with a different revision than the hosts it routes to.

The files do not ship with the gateway. The three that ship today are 36 MB together and would be refreshed by a gateway release rather than by the governance vote that changes them.

- **The first request for a model starts the load and is not checked**, nor is any request until the load lands: the request path never waits on the hub. A load takes about a second.
- **A pin is fetched once.** A new epoch reads the pin again, and fetches only when the commit moved; until the new tokenizer lands the previous one keeps counting.
- **A pinned commit never changes**, so a revision without a `tokenizer.json` (Kimi-K2.6 ships a tiktoken model) or with one this package does not read (`ErrUnsupported`) is recorded and not fetched again. A failed fetch or pin read is retried after `retryInterval` (10 minutes), the stop-token vocabulary's cadence.
- **The library holds one tokenizer per model it was asked about**, and the boundary asks only for a model that routes, so the set is bounded by the served models.

## Counting exactly

`Load` accepts only the byte-level BPE shape it reproduces exactly, and refuses everything else as `ErrUnsupported` rather than count approximately: a `BPE` model without byte fallback, dropout or affixes; an `NFC` normalizer or none; a pre-tokenizer of `Split` steps (`Isolated`, or `Removed`, inverted or not) and a `ByteLevel` step that does not split. `ignore_merges` is honoured: a pre-token the vocabulary holds whole is one token, as GLM's tokenizer requires. The split patterns use lookahead, which RE2 does not have, so they run on `github.com/dlclark/regexp2`.

On the three pinned tokenizers (MiniMax-M2.7, DeepSeek-V4-Flash-0731, GLM-5.3-Flash) `Count` matches Hugging Face `tokenizers` exactly on every text of the comparison: English, Russian, Chinese, Go source, JSON, mixed, and adversarial text, from 200 B to 1 MB. The unit tests pin the same agreement on small tokenizers of each shape (`testdata/`, built with `tokenizers` 0.23.3).

## Estimating a long prompt

Up to `exactBudgetBytes` (64 KiB) in all, `Estimate` counts every part exactly. Above it, it counts `sampleWindows` (8) windows of 8 KiB, one per eighth of the prompt, and scales their tokens per byte to the prompt's length. Each window starts at its own offset inside its eighth, so a prompt that alternates every few kilobytes is not sampled on one side of the alternation only. Parts are counted on their own, as the template separates messages, and a window that spans parts counts each share on its own.

The sampled estimate is what it costs to stay bounded, and it misses on a prompt whose density changes in one place: a dense block the windows land in or miss moves the estimate by its share. On the comparison texts past 64 KiB (English, Russian, Chinese, Go, JSON, mixed and adversarial, up to 4 MB) the mean error was 1.0–1.3% per tokenizer; the worst were +6.9%, a book whose dense table of contents the first window lands in, and −8.7%, a JSON document whose dense stretch the windows miss.

## Bounds

- **Work per call is bounded by the budget**, whatever the prompt's length: at most 64 KiB reach the pre-tokenizer and the merge loop. The worst case measured is a 64 KiB run the pre-tokenizer cannot split (64 KiB of spaces, one pre-token), about 0.12 s on an Apple M4; prose and code of the same size take 10–50 ms.
- **Nothing outlives a call.** A pre-token seen twice in one call is merged once; the memo is dropped when the call returns, so no prompt's words are retained.
- **A `Counter` is immutable once loaded** and is shared by every request.
- **`maxTokenizerBytes`** (64 MiB) bounds one `tokenizer.json` read; the largest pinned today is about 20 MB.
