# vmflow

`vmflow` is a small Go L4 forwarding runtime. It can run as a standalone daemon or be embedded into a larger control plane.

[![Docs](https://img.shields.io/badge/docs-vmflow.bestcheapvps.org-14b8a6)](https://vmflow.bestcheapvps.org)
[![CI](https://github.com/cloudapp3/vmflow/actions/workflows/go.yml/badge.svg)](https://github.com/cloudapp3/vmflow/actions/workflows/go.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/cloudapp3/vmflow.svg)](https://pkg.go.dev/github.com/cloudapp3/vmflow)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Documentation: [Website](https://vmflow.bestcheapvps.org) · [中文说明](https://vmflow.bestcheapvps.org/zh/) · [HTTP API](https://vmflow.bestcheapvps.org/api) · [Docs source](https://github.com/cloudapp3/vmdocs)

## What it does

- TCP, UDP, and `tcp+udp` port forwarding
- Rule lifecycle management: start, stop, restart, and full snapshot apply
- Config-driven daemon with hot reload
- Local admin API for health, rules, stats, precheck, reload, and metrics
- Bearer-token auth with viewer/admin roles
- Structured logs in text or JSON format
- Prometheus-compatible `/metrics`
- Rule precheck for loops, duplicate ports, and unavailable listeners
- Embeddable Go runtime for products that need in-process forwarding
- Terminal dashboard via `vmflow tui`

## Quick start

Install the latest prebuilt binary (Linux/macOS):

```bash
curl -fsSL https://raw.githubusercontent.com/cloudapp3/vmflow/main/install.sh | bash
```

Install globally to `/usr/local/bin`:

```bash
curl -fsSL https://raw.githubusercontent.com/cloudapp3/vmflow/main/install.sh | sudo bash -s -- --dir /usr/local/bin
```

Install a specific release tag:

```bash
curl -fsSL https://raw.githubusercontent.com/cloudapp3/vmflow/main/install.sh | bash -s -- --version v0.1.0
```

The installer downloads GitHub Release archives, verifies `checksums.txt` with SHA-256 by default, and auto-detects an install directory (`/usr/local/bin` → `~/.local/bin` → `~/bin`) when `--dir` is omitted. You can override the install directory with `--dir PATH` or `VMFLOW_INSTALL_DIR`, and skip checksum verification with `--skip-verify` if needed. For private releases or higher GitHub API limits, set `GITHUB_TOKEN` or `GH_TOKEN`.

Or build from source:

```bash
go build -trimpath -o vmflow ./cmd/vmflow
```

Start the daemon in one terminal:

```bash
./vmflow daemon -config ./examples/config.yaml
```

Query it from another terminal:

```bash
./vmflow ctl health
./vmflow ctl rules
./vmflow ctl stats
./vmflow ctl metrics
./vmflow ctl precheck
```

Open the terminal UI:

```bash
./vmflow tui
```

Show build metadata:

```bash
./vmflow version
./vmflow version -json
```

## Configuration

See [`examples/config.yaml`](examples/config.yaml):

```yaml
version: 1
admin_listen_addr: 127.0.0.1:19090

log:
  level: info
  format: text

auth:
  enabled: false
  tokens:
    - name: admin
      token: change-me
      role: admin

rules:
  - rule_id: ssh-forward
    name: ssh-forward
    protocol: tcp
    listen_addr: 0.0.0.0
    listen_port: 2201
    target_addr: 127.0.0.1
    target_port: 22
    enabled: true
```

Security note: keep the admin API on `127.0.0.1` by default. If you expose it outside localhost, enable bearer-token auth and use an admin token for mutating endpoints.

## Commands

```bash
vmflow daemon        -config ./examples/config.yaml [-admin-listen 127.0.0.1:19090]
vmflow ctl           [-addr http://127.0.0.1:19090] [-token TOKEN] <health|rules|stats|metrics|precheck|reload>
vmflow tui           [-addr http://127.0.0.1:19090] [-token TOKEN]
vmflow version       [-json]
```

Aliases are available: `daemon=d`, `ctl=c`, `tui=t`, and `version=v`.

## Admin API

Documented in [`docs/API.md`](docs/API.md). Main endpoints:

- `GET /healthz`
- `GET /v1/rules`
- `GET /v1/stats`
- `GET|POST /v1/precheck`
- `POST /v1/reload`
- `GET /metrics`

## Embedding vmflow

Use the top-level package when vmflow is embedded into another Go service:

```go
rt := vmflow.New()
defer rt.Close()

result := rt.Apply(rules) // []engine.Rule
stats := rt.SnapshotAll()
```

The embedding application owns persistence, auth, UI, audit logs, and business rules. `vmflow` owns only in-process forwarding, rule lifecycle, and real-time counters. See [`docs/EMBEDDING.md`](docs/EMBEDDING.md).

## Development

```bash
make fmt
make test
make vet
make smoke
make build
```

Some tests bind local ports. If your sandbox blocks sockets, run them in an environment that permits local listeners.

## Release

Tagged releases are built by GoReleaser through GitHub Actions:

```bash
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

The release workflow publishes cross-platform archives, `.deb` / `.rpm` packages, and `checksums.txt`. Linux/macOS users can also install with [`install.sh`](install.sh).

## Project layout

- `engine/` — protocol forwarding engine and in-memory stats
- `config/` — YAML config loading and validation
- `controlapi/` — local control API, auth, reload, precheck, metrics wiring
- `metrics/` — Prometheus text exposition helpers
- `precheck/` — static checks before applying rules
- `tui/` — terminal dashboard client
- `cmd/vmflow/` — primary all-in-one binary
- `examples/` — runnable and embeddable examples
- `docs/` — architecture, API, embedding, roadmap, and changelog

## License

MIT
