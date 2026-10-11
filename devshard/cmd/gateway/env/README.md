# `env` — the only place environment variables are read

One typed table: name, type, and nothing else.

## What it owns

- **`Load`** — returns what is **set**, as pointers. A `nil` means the variable was not set; it never means zero.
- **devshardctl's names** — a variable devshardctl had is read under its devshardctl name, parsed the way devshardctl parsed it.
- **`PrivateKey`** — reads a signing key by the **name** of the variable holding it. Errors and log lines name the variable, never the value.

## Boundaries

- **Defaults are not here, with one exception.** `LogFormat` answers `json` for an empty `DEVSHARD_LOG_FORMAT`, because logging is configured before `config` exists. Every other default belongs to [`config`](../config/), which is what makes "unset" and "set to zero" distinguishable.
- **A key is addressed by name, not by value**, everywhere: in `DEVSHARDS_JSON`, in the admin API, and here. A key pasted into a request body would reach the logs and the shell history.
- **An empty value counts as unset**, including for the `NODE_GRPC_URL` / `NODE_RPC_URL` fallbacks of the chain endpoints.
- **Two tables are read elsewhere.** The runtime-params feed ([`../app/runtime_params.go`](../app/runtime_params.go)) reads its own knobs through `devshard/runtimeparams`, because they are fleet-shared `DEVSHARD_*` / `DEVSHARDD_*` names every devshard binary honours under the same spelling. Restating them here would give one knob two owners, which is the failure this package exists to prevent.
- **Tracing reads its own switch the same way.** `DEVSHARD_OTEL_ENABLED`, `OTEL_ENDPOINT` and `OTEL_HEADERS` are read by `devshard/observability` when [`../app/tracing.go`](../app/tracing.go) starts it, because every devshard binary honours them under the same spelling; they are listed in [`../docs/operations.md`](../docs/operations.md) like any other knob.
- **Two grammars.** A devshardctl variable keeps devshardctl's leniency: a value devshardctl would have ignored is ignored with a warning, so an env file that booted devshardctl boots the gateway. A `GATEWAY_*` variable is strict, and every parse failure among them is reported at once, so one restart names every misconfigured variable rather than one per attempt.

## The log format

`LogFormat` reads `DEVSHARD_LOG_FORMAT` apart from `Load`, because the format must be applied before anything can log and `Load` is the first thing that can fail. Empty means `json`, the form a collector reads as labels; any other value than `json` keeps the `log` package's own text line, as devshardctl read it.

## Which names are read

A variable devshardctl had is read under devshardctl's name only — `DEVSHARD_*`, `DEVSHARDS_JSON`, and the few `GATEWAY_*` limits devshardctl already spelled that way. A variable the gateway added is read under its `GATEWAY_*` name. `PrivateKey` reads exactly the variable an escrow record names. See [`docs/operations.md`](../docs/operations.md), "Variable names", for the defaults that differ from devshardctl's and the devshardctl variables that have no counterpart.
