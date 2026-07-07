# Current project summary

## Positioning

`vmflow` is a pure-Go L4 forwarding runtime. It is designed to be used in two modes:

1. a standalone forwarding daemon controlled by a local CLI/API; and
2. an embeddable library inside a larger control plane such as `vmpulse`.

The project is currently a practical v0.1-style MVP: the forwarding path, rule lifecycle, local control plane, observability basics, and embedding API are already in place.

## Implemented capabilities

- TCP forwarding
- UDP forwarding
- `tcp+udp` combined rules
- HTTP/HTTPS-oriented rule plumbing with optional certificate provider support _(当前构建暂未启用)_
- Rule start, stop, restart, remove, and full snapshot apply
- Disabled-rule handling for desired-state snapshots
- Per-rule connection count and traffic counters
- `max_conn` limiting
- Simple per-session speed limiting
- YAML config loading and validation
- Local control API:
  - `GET /healthz`
  - `GET /v1/rules`
  - `GET /v1/stats`
  - `GET|POST /v1/precheck`
  - `POST /v1/reload`
  - `GET /metrics`
- Bearer-token auth with viewer/admin roles
- Structured logs via `slog` in text or JSON format
- Prometheus text metrics
- Rule precheck for loops, port conflicts, and unavailable listeners
- Unified CLI binary: `vmflow daemon`, `vmflow ctl`, `vmflow tui`, `vmflow version`
- Compatibility standalone binaries: `relayd`, `relayctl`, `relaytui`
- Embeddable top-level Go API: `Runtime`, `Options`, `Apply`, `SnapshotAll`, `Close`
- GitHub Actions CI for formatting, tests, vet, smoke checks, build, and release snapshots

## Architecture notes

### Engine layer

`engine/` should stay runtime-only. It owns listeners, proxy loops, sessions, counters, and rule lifecycle. It should not depend on databases, web dashboards, account systems, or product-specific control-plane logic.

### Control layer

`controlapi/`, `metrics/`, `precheck/`, `tui/`, and `config/` provide the standalone daemon control surface. These packages can evolve independently while keeping the engine embeddable.

### Embedding layer

The top-level `github.com/cloudapp3/vmflow` package is the stable embedding facade. Host applications should use it rather than depending on daemon internals.

## Current limits

- Statistics are in-memory only; historical aggregation belongs in an external control plane.
- Reload is safe and prechecked, but there is no graceful drain window yet.
- Speed limiting is intentionally simple and not a shared global bandwidth bucket.
- UDP connection counting is session-like approximation.
- No bundled web dashboard or multi-node coordinator yet.
- No systemd/Docker production packaging yet; release archives ship the binary and docs.

## Submit-readiness checklist

Before pushing to GitHub or cutting a tag:

```bash
make fmt
make test
make vet
make smoke
make build
```

For release validation, run a GoReleaser snapshot:

```bash
goreleaser release --snapshot --clean
```

If the local checkout has an invalid or missing `.git` directory, Go VCS stamping may fail. Use a real Git checkout for final submit/release validation.

## Recommendation

Public releases should promote one primary artifact: `vmflow`. The legacy split commands can remain in source for compatibility, but users should install and run the unified binary.
