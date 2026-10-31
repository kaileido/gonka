# `peerload` — what the other gateways have in flight

Gateways on the chain find each other through a registry contract, exchange per-host load over mutual TLS, and turn it into one shadow value, the discount. The design, the wire contract and the order of the checks are in [`docs/peerload.md`](../docs/peerload.md).

## What it owns

- **The plugin** (`plugin.go`) — the whole channel behind one hook: start, stop, the registry and poll loops. A nil `Plugin` is the switched-off feature.
- **Membership** (`members.go`) — the allowlist from the chain, the registry entries read through `Registry`, and the endpoint grammar the contract applies.
- **Identity and the check** (`identity.go`, `verify.go`) — the self-signed transport certificate, and the mTLS check both sides run.
- **The two ends of a connection** (`server.go`, `poller.go`) — the `GET /v1/peer/{topic}` listener with its request limit, payload cache and connection caps, and one persistent link per peer instance.
- **The wire** (`envelope.go`, `load.go`, `topic.go`, `vocabulary.go`) — the flat protobuf decoders, the `load@1` topic and every value the channel names.
- **The view** (`view.go`) — the latest snapshot per peer instance, and the discount computed from them.

## What it does not own

**It decides nothing.** Only the metrics collector reads the view; no limit, score or route does, and a test parses the gateway's imports to keep it so. Its own load samples, the concurrency it divides by and the dialler come from `app/peers.go`, so `peerload` imports neither `limits` nor `perf`. The chain calls live in `chain/registry.go`.
