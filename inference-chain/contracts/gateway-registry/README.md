# Gateway Registry Contract

CosmWasm registry of gateway peer instances: each allowlisted address publishes the endpoint, transport key fingerprint, protocol and topics of its instances.

## Rules

- `RegisterInstance` is writer only and upserts one instance of the sender, at most once per `min_update_interval_blocks` per instance and at most `max_instances_per_address` per address.
- An instance is expired when `height - updated_height >= instance_ttl_blocks` (16200). Queries skip expired instances at once; they are deleted lazily, up to 10 by each `RegisterInstance` or up to 100 by `PruneExpired`, oldest first.
- `SyncWriters`, `SetPaused` and `SetOwner` are owner only. `SyncWriters` deletes all instances of removed addresses.

## Build

`make build` runs the CosmWasm optimizer in Docker. `cargo test` runs the cw-multi-test suite. `cargo run --bin schema` writes the JSON schema to `schema/`.
