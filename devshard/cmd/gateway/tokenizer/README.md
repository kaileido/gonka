# `tokenizer` — a prompt's tokens, before anything is sent

Counts a chat prompt's tokens with the tokenizer the chain pins for the model, once per request, so the boundary can refuse a prompt the model's context cannot hold before a nonce exists and carry the same count to the limiter and the ledgers ([`docs/request.md`](../docs/request.md), "Step 6 in detail").

## What it owns

- `bpe.go` — `Load` reads a Hugging Face `tokenizer.json` into a `Counter`; `Counter.Count` is its exact token count of a text, without the special tokens a chat template adds.
- `added.go` — the file's `added_tokens`, found in a text before it is normalized and pre-tokenized, each one token.
- `merge.go` — the merge loop: lowest rank first, leftmost on a tie, as Hugging Face BPE applies merges. A piece of up to `scanMergeBytes` (24) bytes is merged by scanning its few symbols, a longer one with a heap of candidate pairs.
- `pairs.go` — `pairTable`: the merge of a pair of tokens, in an open-addressed table with a bit filter in front of it.
- `pretokenize.go` — hand-written scanners for the split patterns the pinned tokenizers use, found by the pattern's `canonicalPattern` spelling (raw CR and LF as `\r` and `\n`, `\"` as `"`), so DeepSeek-V4's raw line breaks select the same scanner as the escaped form. A rule with a scanner keeps its compiled regexp2 pattern too, as the fallback and the tests' reference.
- `estimate.go` — `Counter.EstimateTokens`: a prompt's tokens, exact up to a budget and estimated from samples beyond it; `Estimate.Minimum` and `Counter.MinimumTokens`: the fewest tokens it holds, the exact count or a lower bound from the samples.
- `library.go` — `Library.Counter`: the counter for each model, loaded in the background and fetched again when the revision the chain pins moves.
- `vocabulary.go` — the `tokenizer.json` names `Load` reads and the hub address.

The package does not read chat bodies: the composition root (`app/prompt_tokens.go`) turns a normalized body into its text with `filters.PromptText` and counts it once with `Counter.EstimateTokens`.

## Where the tokenizer comes from

The epoch's model snapshot pins every model to a Hugging Face repo and commit (`hf_repo`, `hf_commit`), the revision hosts serve and validators replay. `Library` reads that pin through `vocabulary.ChainModelSource`, the same chain query the stop-token vocabulary reads (`app/stop_token_vocabulary.go`), and fetches `tokenizer.json` at that commit. A model id never selects a tokenizer by name, so a new model is counted the epoch governance registers it, and a gateway is never counting with a different revision than the hosts it routes to.

The files do not ship with the gateway: the three in use today are 36 MB together, and shipping them would tie a tokenizer change to a gateway release rather than to the governance vote that makes it.

- **The first request for a model starts the load and is not checked**, nor is any request until the load lands: the request path never waits on the hub. A load takes about a second. Before the gateway knows the epoch, no pin is read.
- **A pin is fetched once.** A new epoch reads the pin again, and fetches only when the commit moved; while the new tokenizer loads the previous one keeps counting. When the new one fails to load, the previous one is dropped and the model goes unchecked until the retry lands, rather than count with a vocabulary the hosts no longer serve; a pin read that fails keeps the loaded tokenizer, since the pin it read is still the last one known.
- **A pinned commit never changes**, so a revision without a `tokenizer.json` (Kimi-K2.6 ships a tiktoken model) or with one this package does not read (`ErrUnsupported`) is recorded and not fetched again; so is a file that was read whole and does not load, or is larger than `maxTokenizerBytes`, and a parse that panics, which is recovered in the background load and logged as an error rather than crash the gateway on every restart. A failed fetch or pin read is retried after `retryInterval` (10 minutes), the stop-token vocabulary's cadence.
- **The library holds one tokenizer per model it was asked about**, and the boundary asks only for a model that routes, so the set is bounded by the served models.

## Counting exactly

`Load` accepts only the byte-level BPE shape it reproduces exactly, and refuses everything else as `ErrUnsupported` rather than count approximately: a `BPE` model without byte fallback, dropout or affixes; an `NFC` normalizer or none; a pre-tokenizer of `Split` steps (`Isolated`, or `Removed`, inverted or not) and a `ByteLevel` step that does not split. `ignore_merges` is honoured: a pre-token the vocabulary holds whole is one token, as GLM's tokenizer requires. The split patterns use lookahead, which RE2 does not have, so a pattern without a scanner runs on `github.com/dlclark/regexp2`; every testdata and pinned pattern has one. Every compiled pattern carries `splitMatchTimeout` (500 ms) per match, so a pattern that backtracks without end fails the count with an error, which leaves the request unchecked, instead of holding it. The slowest single match measured on the pinned patterns over 64 KiB (spaces, letters, punctuation, CJK) took 9 ms on a loaded Apple M4; regexp2 reads time from a clock that ticks every 100 ms, and a timeout near the tick fires on a long match under CPU load. The type and behaviour names `Load` reads are in `vocabulary.go`.

- **Bytes.** A byte-level vocabulary is written in GPT-2's printable stand-in for every byte (`byteSymbols`): the printable ASCII and Latin-1 bytes stand for themselves, every other byte for a code point from 256 up. Every byte must have a token, or the file is unsupported.
- **Merges** are read in both spellings tokenizer.json uses, `["left", "right"]` and `"left right"`, and applied lowest rank first, leftmost on a tie.
- **Splits.** `Isolated` keeps both the matches and the text between them; `Removed` drops the matches; an inverted `Removed` keeps only the matches. A literal `String` pattern is escaped, not compiled as a regex.

On the three pinned tokenizers (MiniMax-M2.7, DeepSeek-V4-Flash-0731, GLM-5.3-Flash) `Count` matches Hugging Face `tokenizers` exactly on every text of the comparison: English, Russian, Chinese, Go source, JSON, mixed, and adversarial text, from 200 B to 1 MB. The unit tests pin the same agreement on small tokenizers of each shape (`testdata/`, built with `tokenizers` 0.23.3).

### Added tokens

A `tokenizer.json` lists `added_tokens` (DeepSeek-V4-Flash 1,283, MiniMax-M2.7 54, GLM-5.3-Flash 36, Qwen2.5 22): `<think>`, `<minimax:tool_call>`, `<｜User｜>` and the like. Hugging Face `tokenizers` finds them before anything else (`AddedVocabulary::extract_and_normalize`, 0.23.3), so a host that encodes the rendered prompt with it counts a literal `<think>` in a message as one token, not the five or six its bytes merge to. `Count` does the same:

- **Raw entries** (`normalized: false`) are matched in the text as given; **normalized entries** are matched in each gap between raw matches after that gap is normalized, by their normalized content. A gap normalizes on its own, as Hugging Face normalizes it.
- **Matching** is leftmost first and longest at the same start, the trie's `LeftmostLongest`, over bytes, without overlaps.
- **`lstrip` and `rstrip`** widen a match over the Unicode `White_Space` run before or after it, Rust's `\s`, which is Go's `unicode.IsSpace`.
- **Every match is one token**; the text between matches is pre-tokenized and merged as before. An entry repeated in the file keeps its last flags, as Hugging Face's map by content does, and an empty one is skipped.
- **Refused as `ErrUnsupported`:** a `single_word` entry, because its word test is Rust regex's Unicode `\w`, whose tables Go does not carry at the same version; and two normalized entries with the same normalized content, which Hugging Face's trie refuses to build. No pinned file has either.

On MiniMax-M2.7, DeepSeek-V4-Flash-0731, GLM-5.3-Flash and Qwen2.5, `Count` matches Hugging Face `tokenizers` 0.23.3 on 26,135 texts, 22,775 of them holding added tokens: every entry of each file at the start, middle and end, adjacent, inside words, between spaces and line breaks, before a mark NFC composes with it, cut short by a byte at either end, and random mixes of entries and ordinary text. `TestCountSplitsOnAddedTokensAsHuggingFace` pins the same agreement on two testdata tokenizers that carry added tokens with every flag but `single_word`. `TestIDsMatchTheOriginal` compares with the original implementation, which counted added tokens as ordinary text, only the texts that hold none of the tokenizer's added tokens.

## Estimating a long prompt

Up to `exactBudgetBytes` (64 KiB) in all, `EstimateTokens` counts every part exactly. Above it, it counts `sampleWindows` (8) windows of 8 KiB, one per eighth of the prompt, and scales their tokens per byte to the prompt's length. The windows spread over their eighths: the first starts its eighth, the last ends its eighth, and the others sit evenly between, so together they sample every part of an eighth, and a prompt whose density changes inside each eighth is not sampled on one side of the change only. The same text always gets the same windows. Parts are counted on their own, as the template separates messages, and a window that spans parts counts each share on its own.

The sampled estimate is what it costs to stay bounded, and it misses on a prompt whose density changes in one place: a dense block the windows land in or miss moves the estimate by its share, and a prompt that repeats with a period near the windows' step is sampled unevenly. With the windows spread, on seven real texts (two English books, Russian Wikipedia, a Chinese novel, Go and Python source; 256 KiB to 2.6 MB) the mean error is 1.6–2.1% per tokenizer and the worst are +6.3% and −4.6%; on `BenchmarkEstimate`'s generated texts (256 KiB to 4 MiB) the worst are +2.1% and −6.1%, the latter on blocks of five kinds that repeat every 20 KiB. Before they were spread, the worst over the same texts were +4.9% and −3.7%, and on the comparison texts of the first measurement +6.9%, a book whose dense table of contents the first window lands in (it still starts the prompt), and −8.7%, a JSON document whose dense stretch the windows miss; those two texts were not measured again.

The estimate is the count a request carries to the limiter and the ledgers, unbiased because it is summed over many requests. `Estimate.Minimum` answers the check that refuses a prompt, and an over-count refuses a request a host would serve, so past the budget it divides the estimate by 1 + `sampledOvercountPercent` (7%), the worst over-count measured rounded up (+6.3% with the windows spread, +6.9% before). What it returns is then at most the prompt's real count on every text measured; an under-count is left as it is, because it only leaves a request unchecked.

## Bounds

- **Work per call is bounded by the budget**, whatever the prompt's length: at most 64 KiB reach the pre-tokenizer and the merge loop. The worst case measured is a 64 KiB run the pre-tokenizer cannot split (64 KiB of spaces, one pre-token), 30–75 ms on a loaded Apple M4; mixed prose, code and CJK of the same size take 2–5 ms. Finding added tokens walks the trie from each byte that starts one, as deep as the text follows an entry; its worst measured cost is 64 KiB of an unclosed DeepSeek placeholder prefix, 2.0 ms without added tokens and 4.5 ms with them.
- **Nothing outlives a call.** A pre-token of up to `maxRememberedPiece` (64) bytes is remembered in a direct-mapped table of at most `maxMemoSlots` (65,536) slots, so a word that repeats within a text is usually merged once, and one that collides with another forgets it and is merged again; longer pre-tokens rarely repeat and are not remembered. The table is cleared when the call returns and pooled for the next call, so no prompt's words are retained.
- **A `Counter` is immutable once loaded** and is shared by every request.
- **`maxTokenizerBytes`** (64 MiB) bounds one `tokenizer.json` read; the largest pinned today is about 20 MB.

## Benchmarks

`BenchmarkEstimate` and `BenchmarkLoad` here, and `BenchmarkChatContextCheck` in `api/`, run on real tokenizers and skip without them. `GATEWAY_TOKENIZER_DIR` names a directory of `tokenizer.json` files under any name ending in `.json`; each is one tokenizer. From `devshard/`:

```sh
GATEWAY_TOKENIZER_DIR=/path/to/tokenizers go test ./cmd/gateway/tokenizer/ -run '^$' -bench 'Load|Estimate' -benchtime 10x
GATEWAY_TOKENIZER_DIR=/path/to/tokenizers go test ./cmd/gateway/api/ -run '^$' -bench ChatContextCheck -benchtime 20x
```

`BenchmarkEstimate` reports each estimate's error against the exact count of the same text as `%err`.

`BenchmarkCount` times `Count` on Qwen2.5 (`qwen.json` in `GATEWAY_TOKENIZER_DIR`) from 200 B to 1 MiB. `TestIDsMatchTheOriginal` and `FuzzIDsMatchTheOriginal` compare the pre-tokens and full token IDs with the original implementation frozen in `reference_test.go` (regexp2 split, heap merge) on every testdata tokenizer and every one in `GATEWAY_TOKENIZER_DIR`, plus windows of the `.txt` files in `GATEWAY_BENCH_CORPUS_DIR`. `GATEWAY_TOKENIZER_SWEEP=1` adds the sweep over every Unicode scalar value, which takes minutes. `TestConcurrentCountsMatchSequentialOnes` counts every testdata tokenizer and text from eight goroutines at once, so the pooled memo tables pass between calls, and compares with a single-threaded run; it passes under `go test -race`.
