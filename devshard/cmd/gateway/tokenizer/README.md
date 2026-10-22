# `tokenizer` — a prompt's tokens, before anything is sent

Counts a chat prompt's tokens with the tokenizer the chain pins for the model, so the boundary can refuse a prompt the model's context cannot hold before a nonce exists ([`docs/request.md`](../docs/request.md), "Step 6 in detail").

## What it owns

- `bpe.go` — `Load` reads a Hugging Face `tokenizer.json` into a `Counter`; `Counter.Count` is its exact token count of a text, without the special tokens a chat template adds.
- `merge.go` — the merge loop: lowest rank first, leftmost on a tie, as Hugging Face BPE applies merges.
- `estimate.go` — `Counter.MinimumTokens`: the fewest tokens a prompt holds, exact up to a budget and a lower bound from samples beyond it; `Counter.MaximumTokens`: a bound it never exceeds, read from the prompt's bytes without tokenizing.
- `library.go` — `Library.Counter`: the counter for each model, loaded in the background and fetched again when the revision the chain pins moves.
- `vocabulary.go` — the `tokenizer.json` names `Load` reads and the hub address.

The package does not read chat bodies: the composition root (`app/prompt_tokens.go`) turns a normalized body into its text with `filters.PromptText` and bounds it with `Counter.MaximumTokens` and `Counter.MinimumTokens`.

## Where the tokenizer comes from

The epoch's model snapshot pins every model to a Hugging Face repo and commit (`hf_repo`, `hf_commit`), the revision hosts serve and validators replay. `Library` reads that pin through `vocabulary.ChainModelSource`, the same chain query the stop-token vocabulary reads (`app/stop_token_vocabulary.go`), and fetches `tokenizer.json` at that commit. A model id never selects a tokenizer by name, so a new model is counted the epoch governance registers it, and a gateway is never counting with a different revision than the hosts it routes to.

The files do not ship with the gateway: the three in use today are 36 MB together, and shipping them would tie a tokenizer change to a gateway release rather than to the governance vote that makes it.

- **The first request for a model starts the load and is not checked**, nor is any request until the load lands: the request path never waits on the hub. A load takes about a second. Before the gateway knows the epoch, no pin is read.
- **A pin is fetched once.** A new epoch reads the pin again, and fetches only when the commit moved; until the new tokenizer lands the previous one keeps counting.
- **A pinned commit never changes**, so a revision without a `tokenizer.json` (Kimi-K2.6 ships a tiktoken model) or with one this package does not read (`ErrUnsupported`) is recorded and not fetched again; so is a file that was read whole and does not load, or is larger than `maxTokenizerBytes`. A failed fetch or pin read is retried after `retryInterval` (10 minutes), the stop-token vocabulary's cadence.
- **The library holds one tokenizer per model it was asked about**, and the boundary asks only for a model that routes, so the set is bounded by the served models.

## Counting exactly

`Load` accepts only the byte-level BPE shape it reproduces exactly, and refuses everything else as `ErrUnsupported` rather than count approximately: a `BPE` model without byte fallback, dropout or affixes; an `NFC` normalizer or none; a pre-tokenizer of `Split` steps (`Isolated`, or `Removed`, inverted or not) and a `ByteLevel` step that does not split. `ignore_merges` is honoured: a pre-token the vocabulary holds whole is one token, as GLM's tokenizer requires. The split patterns use lookahead, which RE2 does not have, so they run on `github.com/dlclark/regexp2`. The type and behaviour names `Load` reads are in `vocabulary.go`.

- **Bytes.** A byte-level vocabulary is written in GPT-2's printable stand-in for every byte (`byteSymbols`): the printable ASCII and Latin-1 bytes stand for themselves, every other byte for a code point from 256 up. Every byte must have a token, or the file is unsupported.
- **Merges** are read in both spellings tokenizer.json uses, `["left", "right"]` and `"left right"`, and applied lowest rank first, leftmost on a tie.
- **Splits.** `Isolated` keeps both the matches and the text between them; `Removed` drops the matches; an inverted `Removed` keeps only the matches. A literal `String` pattern is escaped, not compiled as a regex.

On the three pinned tokenizers (MiniMax-M2.7, DeepSeek-V4-Flash-0731, GLM-5.3-Flash) `Count` matches Hugging Face `tokenizers` exactly on every text of the comparison: English, Russian, Chinese, Go source, JSON, mixed, and adversarial text, from 200 B to 1 MB. The unit tests pin the same agreement on small tokenizers of each shape (`testdata/`, built with `tokenizers` 0.23.3).

## Estimating a long prompt

Up to `exactBudgetBytes` (64 KiB) in all, `MinimumTokens` counts every part exactly. Above it, it counts `sampleWindows` (8) windows of 8 KiB, one per eighth of the prompt, and scales their tokens per byte to the prompt's length. Each window starts at its own offset inside its eighth, `windowPhaseStep` (half a window) further than the window before it, so a prompt that alternates every few kilobytes is not sampled on one side of the alternation only. Parts are counted on their own, as the template separates messages, and a window that spans parts counts each share on its own.

The sampled estimate is what it costs to stay bounded, and it misses on a prompt whose density changes in one place: a dense block the windows land in or miss moves the estimate by its share. On the comparison texts past 64 KiB (English, Russian, Chinese, Go, JSON, mixed and adversarial, up to 4 MB) the mean error was 1.0–1.3% per tokenizer; the worst were +6.9%, a book whose dense table of contents the first window lands in, and −8.7%, a JSON document whose dense stretch the windows miss.

`MinimumTokens` answers the check that refuses a prompt, and an over-count refuses a request a host would serve, so past the budget it divides the estimate by 1 + `sampledOvercountPercent` (7%), the worst over-count measured rounded up. What it returns is then at most the prompt's real count on every comparison text; an under-count is left as it is, because it only leaves a request unchecked.

## The byte bound

`MaximumTokens` is what a prompt's tokens can be at most, from its length alone, so the context check tokenizes only a prompt that bound does not clear. A pre-token is a stretch of the text, pre-tokens do not overlap, and the merge loop starts from one symbol per byte and only joins symbols, so `Count` of a text is at most the UTF-8 length of what it tokenizes: the text itself, or its NFC form for a tokenizer that normalizes. A special token's spelling in a message is counted as ordinary text, so the bound holds for it too; the chat template's own tokens are not part of either count.

- **Up to `exactBudgetBytes`** `MinimumTokens` is the sum of each part's `Count`, so the bound is the parts' bytes, after NFC for a tokenizer that normalizes.
- **Past it** `MinimumTokens` scales the sampled windows' tokens per byte to the prompt's raw length, so the bound is the raw length times the most a window's text can grow before it is counted: 1, or `nfcMaxExpansion` (3, the most NFC grows UTF-8 by, UAX #15) for a tokenizer that normalizes a prompt that is not all ASCII. A window of nothing but U+1D160, which NFC turns from 4 bytes into 12, comes close to it.

The bound is loose, since a token usually covers several bytes, and costs one pass over the text, so a prompt is tokenized only when `max_tokens` and the template leave it less room in the context than its bytes.

## Bounds

- **Work per call is bounded by the budget**, whatever the prompt's length: at most 64 KiB reach the pre-tokenizer and the merge loop. The worst case measured is a 64 KiB run the pre-tokenizer cannot split (64 KiB of spaces, one pre-token), about 0.12 s on an Apple M4; prose and code of the same size take 10–50 ms.
- **Nothing outlives a call.** A pre-token of up to `maxRememberedPiece` (64) bytes seen twice in one call is merged once, since words repeat within a text and longer pre-tokens rarely do; the memo is dropped when the call returns, so no prompt's words are retained.
- **A `Counter` is immutable once loaded** and is shared by every request.
- **`maxTokenizerBytes`** (64 MiB) bounds one `tokenizer.json` read; the largest pinned today is about 20 MB.

## Benchmarks

`BenchmarkEstimate` and `BenchmarkLoad` here, and `BenchmarkChatContextCheck` in `api/`, run on real tokenizers and skip without them. `GATEWAY_TOKENIZER_DIR` names a directory of `tokenizer.json` files under any name ending in `.json`; each is one tokenizer. From `devshard/`:

```sh
GATEWAY_TOKENIZER_DIR=/path/to/tokenizers go test ./cmd/gateway/tokenizer/ -run '^$' -bench 'Load|Estimate' -benchtime 10x
GATEWAY_TOKENIZER_DIR=/path/to/tokenizers go test ./cmd/gateway/api/ -run '^$' -bench ChatContextCheck -benchtime 20x
```

`BenchmarkEstimate` reports each estimate's error against the exact count of the same text as `%err`.
