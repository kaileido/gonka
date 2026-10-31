# The peer channel

Gateways on the chain find each other through a registry contract and exchange per-host load over mutual TLS. The load feeds one shadow value, the discount, which reaches `/metrics` and nothing else. The package is `peerload`.

## Dark mode

The channel is off unless `DEVSHARD_PEER_ENABLED` is true **and** `DEVSHARD_PEER_REGISTRY_CONTRACT` is set. Off, `app/peers.go` returns a nil plugin before it reads any key or the chain: there is no listener, no dial, no goroutine, no subscriber and no metric family.

On, the channel runs in shadow. It serves and polls, and computes the discount, and `limits.Capacity.EscrowWeight`, the scheduler and the limiters read none of it. `app/peers.go` reads this gateway's own state for it, so `peerload` imports neither `limits` nor `perf`, and only the metrics collector reads the view; `peerload/boundary_test.go` parses the gateway's imports to keep it that way.

When switched on, `DEVSHARD_PEER_INSTANCE_ID`, `DEVSHARD_PEER_ADVERTISE_URL` and `DEVSHARD_PEER_IDENTITY_KEY_ENV` are required; a missing or invalid one refuses the boot. So does a listener that cannot bind. The advertised URL is held to the contract's own grammar: `https://host:port`, at most 256 bytes, a host of letters, digits, `.` and `-` or a bracketed IPv6, a port from 1 to 65535. The gateway takes 256 as the contract's `max_endpoint_len`; a contract set lower still refuses the write. The [contract's README](../../../../inference-chain/contracts/gateway-registry/README.md) covers deploying it with an admin, upgrading it and changing its limits.

## The transport key

TLS in Go has no secp256k1, so the certificate carries an ed25519 key derived from the chain key that `DEVSHARD_PEER_IDENTITY_KEY_ENV` names: the seed is HKDF-SHA256 of the chain key, with no salt and the info `gonka/gateway/peer-transport/v1/<address>/<instance_id>`. The same key and instance id give the same transport key across restarts, so the registered fingerprint stays valid. Another instance id gives another key, and the derivation does not lead back to the chain key. Changing the chain key or the instance id changes the fingerprint, and the instance writes its new entry at the next registry read.

## Membership

The allowlist is the chain parameter `DevshardEscrowParams.allowed_creator_addresses`, read with every chain poll as `chain.PhaseSnapshot.AllowedCreators`. It is authoritative: the contract only says how to reach and how to recognise an address's instances. An allowlist that is empty or was never read admits nobody, unlike the chain, where an empty list admits everyone.

The peers are the registry entries of allowlisted addresses, unexpired, minus this instance. An entry expires 16200 blocks after its `updated_height`; the contract hides an expired entry from its queries, and the gateway applies the same rule itself because it keeps serving from its last good read when the contract cannot be reached.

Replicas of this gateway's address are peers. This instance is never one: an entry is excluded when it has the same address and instance id, the same key fingerprint, or the same endpoint as `DEVSHARD_PEER_ADVERTISE_URL`. So a stale entry left under an old instance id cannot make an instance dial itself.

The registry is read at start, every 300 s, and on any change of the allowlist, at most 100 addresses per call, each read within 10 s and one refresh within 60 s. Until a read of the allowlist has returned once, the next attempt comes after 10 s instead of 300 s. The chain does not validate allowlist addresses, and one the contract cannot parse fails its whole batch, so a failed batch is read again per address: an address that still fails keeps its last good entries and the others follow the chain, and registration goes on. If every address fails the whole last good read stays and nothing is written. An unknown instance or fingerprint at a handshake rereads that one address, at most once per 30 s per address.

Each instance writes its own entry with `RegisterInstance`, an unordered transaction signed by the key `DEVSHARD_PEER_IDENTITY_KEY_ENV` names: when it is missing or differs, and after 4050 blocks (about 6 h) to keep it from expiring. The write has the commit poll plus 15 s to finish. After a failed write the next waits 300 s, doubling up to 6 h, and a success clears the wait; a write that a failed transaction refuses still pays its fee, so it must not repeat at every read.

## The order of the checks

Both sides run the same check once per connection, as `tls.Config.VerifyConnection`, TLS 1.3 only, with no session resumption and no CA: certificates are self-signed and their dates are not read. Trust is the registry pin.

1. Exactly one certificate, carrying exactly one URI SAN `gonka:<address>/<instance_id>`.
2. It is not this instance, by name or by key.
3. The address is in the allowlist now.
4. SHA-256 of the certificate's `RawSubjectPublicKeyInfo` equals the registry entry of exactly that instance. On a miss the address is reread once, under the rate limit, and the check repeats; a reread never turns into an accept by itself.
5. The entry's protocol is supported.

The dialling side also requires the certificate to name the instance it dialled. The registry accepts any host, so a peer's address goes through the dial guard that refuses private and link-local addresses, unless `DEVSHARD_ALLOW_PRIVATE_ADDRESSES` lifts it.

TLS runs `VerifyConnection` before it checks the peer's signature, so a refusal is counted there and `accepted` only once the connection is past its handshake and inside its address's cap.

Every request repeats steps 1 to 5 from the connection's state, without touching the registry, so a request off a verified connection or from a peer removed a moment ago answers 403. A poller applies these to a response before it reads the payload, in order: the body is read no further than the topic's cap; `topic` and `topic_version` match the request; `origin` and `instance` match the connection; `issued_at_ms` is within 30 s of now each way; `seq` is greater than the last one seen.

### Removal

Each registry refresh, which an allowlist change triggers at once, closes server connections whose peer no longer passes and drops the poller's link to it, along with its snapshot. A new key under the same instance id is a new link.

## Limits on the listener

Every request for a known topic, whatever its answer, spends one allowance held per peer instance, by the name in its verified certificate, and per topic: one request per topic interval, with a burst of 3, so a restart or a retry passes and a peer asking faster answers 429 until it slows down. A request for anything else holds no allowance and answers 404, which costs no more to answer than a 429, so a peer cannot grow the set of allowances by naming topics. A peer's allowances are forgotten at the registry refresh that finds it gone.

A topic's payload is built at most once per second, and concurrent requests wait for that one build. Each response is its own envelope around the shared payload: `seq` is new every time, so a second poll inside the second still passes the strictly increasing check, and `issued_at_ms` is when the payload was built, so its age stays honest. A failed build is not kept; the next request builds again.

The listener holds at most 1024 connections, room for 64 addresses of 16 instances each, and closes any further one at accept, before its handshake. That count includes connections not yet identified: a connection is identified when its HTTP/2 preface arrives, after a handshake of at most 5 s of reads plus up to 5 s for one registry reread, and at most 10 s after that handshake. One remote IP holds at most 8 connections not yet identified and any further one from it is closed at accept, so one source cannot fill the listener. Peers behind one IP stay inside 8 because a handshake and a preface take moments, and one that is refused connects again at its next poll. Filling an empty listener takes 128 sources, so a distributed flood can still keep new peers out while it lasts; this risk remains, and established connections keep working. One address holds at most 16 identified connections, the `max_instances_per_address` the contract is deployed with, and any further one is closed after its HTTP/2 preface, at most 10 s after its handshake, and stops counting against its address at once. The gateway keeps its own copy: past 16, an address's further instances are not peers, so raising the limit on the contract needs a gateway release first. A replacement instance can be refused until the old connection's 60 s idle timeout frees its place; its polls fail meanwhile and the channel recovers without action.

## Channel contract

`GET /v1/peer/{topic}`, HTTP/2, no request body. `/v1/` is `Instance.protocol` 1; an incompatible channel means `/v2/` and a new protocol number. A registry entry declares its topics as `name@version`, and a peer is polled only for the topics both sides have.

| Answer | When |
| --- | --- |
| 200, `application/x-protobuf`, an Envelope | the topic is known |
| 403, `Gonka-Peer-Error: refused` | the connection does not pass the check |
| 404, `Gonka-Peer-Error: unknown_topic`, empty body | the topic is unknown |
| 400, `Gonka-Peer-Error: body_forbidden` | the request carries a body |
| 405 | any other method |
| 429, `Gonka-Peer-Error: rate_limited` | the peer is over its request allowance |
| 500, empty body | the topic could not be built within its cap |

A new topic is a `Topic` in the gateway release, and needs no contract change. Each sets its name, version, byte cap, interval and maximum age.

Topic `load@1` serves this instance's own state: at most 64 KiB, 512 rows, 32 models and 128-byte strings, every 10 s, for at most 30 s. A row past a cap is dropped, the least busy first. A received payload past a cap is refused whole. In-flight counts round up to a multiple, 2 requests and 2048 tokens by default, so a peer sees less of the sender's volume, and saturate at the largest multiple a uint32 holds.

### Wire schema

Flat protobuf, encoded and decoded by hand in `peerload`: a group is refused, because a peer's bytes never reach a recursive parser. An unknown field is skipped.

| Message | Field | Number | Type |
| --- | --- | --- | --- |
| Envelope | `topic` | 1 | string |
| | `topic_version` | 2 | uint32 |
| | `origin`, the address | 3 | string |
| | `seq`, unix nanoseconds, strictly increasing | 4 | uint64 |
| | `issued_at_ms` | 5 | int64 |
| | `payload` | 8 | bytes |
| | `instance` | 9 | string |
| LoadPayload | `interval_ms` | 1 | uint32 |
| | `models`, the index table | 2 | repeated string |
| | `hosts` | 3 | repeated HostLoad |
| HostLoad | `participant` | 1 | string |
| | `model`, an index into `models` | 2 | uint32 |
| | `inflight_requests` | 3 | uint32 |
| | `inflight_input_tokens` | 4 | uint32 |
| | `inflight_output_tokens` | 5 | uint32 |
| | `cutoff`: 0 closed, 1 half open, 2 open | 6 | uint32 |
| | `ejected` | 7 | bool |
| | `pressure_first_content`, 0 is unknown | 8 | float |
| | `pressure_decode`, 0 is unknown | 9 | float |

Numbers 6 and 7 of Envelope are reserved for `chain_height` and `epoch`, and 15 for `origin_sig`.

## The discount

```
load(a,p,m)  = sum over fresh instances i of address a  inflight_requests[i,p,m]
foreign(p,m) = sum over addresses a  min(load(a,p,m), conc(p,m))
d(p,m)       = clamp(1 - foreign(p,m) / conc(p,m), 0.25, 1)
```

`conc(p,m)` is what this gateway itself computes the host earns: `limits.EarnedConcurrency` of `Limits.Concurrency.RequestsPer10000Weight` and `Capacity.ParticipantWeight`. An address is summed before it is clamped, so replicas give it no extra weight. A pair with no known concurrency, or no fresh peer, is 1. A snapshot stops counting 30 s after the earlier of its `issued_at_ms` and its arrival. The view holds one snapshot per peer, and a peer's rows only produce a metric series for a pair this gateway has a concurrency for, which needs a model its chain view names, so a peer cannot grow the label set.

`inflight_requests` comes from `perf.HostState.ModelInflight`, the requests in flight to that host on that model. A pair `perf` has no sample of yet has no row.
