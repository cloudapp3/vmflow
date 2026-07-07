# vmflow

**High-performance L4 network forwarding, in pure Go.**

Route TCP and UDP traffic with a production-grade forwarding runtime that runs as a standalone daemon or drops straight into your control plane. Hot-reloadable rules, live metrics, and an embeddable core.

[![Docs](https://img.shields.io/badge/docs-vmflow.bestcheapvps.org-14b8a6)](https://vmflow.bestcheapvps.org)
[![CI](https://github.com/cloudapp3/vmflow/actions/workflows/go.yml/badge.svg)](https://github.com/cloudapp3/vmflow/actions/workflows/go.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/cloudapp3/vmflow.svg)](https://pkg.go.dev/github.com/cloudapp3/vmflow)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Documentation: [Website](https://vmflow.bestcheapvps.org) · [中文说明](https://vmflow.bestcheapvps.org/zh/) · [HTTP API](https://vmflow.bestcheapvps.org/api) · [Docs source](https://github.com/cloudapp3/vmdocs)

> **完整使用指南:** [`docs/USAGE.md`](docs/USAGE.md) —— 覆盖安装、配置、运维、远程访问(TLS/mTLS/Cloudflare)、安全加固与排错。English quick reference is below; the deep guide is in `docs/`.

## What it does

- TCP, UDP, and `tcp+udp` port forwarding
- Rule lifecycle management: start, stop, restart, and full snapshot apply
- Config-driven daemon with hot reload
- Local control API for health, rules, stats, precheck, reload, and metrics
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
control_listen_addr: 127.0.0.1:19090
# control_tls:                       # enable TLS on the control API
#   cert_file: /etc/vmflow/control.crt
#   key_file: /etc/vmflow/control.key
#   client_ca_file: clients-ca.crt   # optional: require client certs (mTLS)
#   min_version: "1.2"               # "1.2" (default) | "1.3"

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

Security note: the daemon **refuses to start** if the control API is bound to a non-loopback address without auth. Keep it on `127.0.0.1` (the default), or enable `auth` before exposing it. To bind remotely without auth anyway, pass `--insecure-allow-remote-control` (not recommended — put it behind a TLS-terminating reverse proxy instead).

## Commands

```bash
vmflow daemon        -config ./examples/config.yaml [-control-listen 127.0.0.1:19090] [-insecure-allow-remote-control]
vmflow ctl           [-addr http://127.0.0.1:19090] [-token TOKEN] <health|rules|stats|metrics|precheck|reload>
vmflow tui           [-addr http://127.0.0.1:19090] [-token TOKEN]
vmflow version       [-json]
vmflow update        [--check] [--version tag]
```

Aliases are available: `daemon=d`, `ctl=c`, `tui=t`, `version=v`, and `update=u`.

## Control API

Documented in [`docs/API.md`](docs/API.md). Main endpoints:

- `GET /healthz`
- `GET /v1/rules`
- `GET /v1/stats`
- `GET|POST /v1/precheck`
- `POST /v1/reload`
- `GET /metrics`

## Control API TLS

The control API is plain HTTP by default. Serve it over TLS by setting `control_tls.cert_file` and `key_file`:

```yaml
control_tls:
  cert_file: /etc/vmflow/control.crt
  key_file: /etc/vmflow/control.key
  client_ca_file: /etc/vmflow/clients-ca.crt   # optional → mTLS (clients must present a cert)
  min_version: "1.2"   # "1.2" (default) | "1.3"
```

Clients then use `https://` for `-addr`. For a private/self-signed CA pass `--tls-ca-file` (or `VMFLOW_TLS_CA_FILE`); for mTLS also pass `--tls-client-cert` / `--tls-client-key` (or `VMFLOW_TLS_CLIENT_*`):

```bash
vmflow ctl -addr https://host:19090 -tls-ca-file ca.crt health
```

With `client_ca_file` (mTLS) set, the control API counts as authenticated for the non-loopback fail-closed rule, so it can be exposed without bearer auth. For public exposure, binding loopback behind a TLS-terminating reverse proxy (Caddy/Nginx + ACME) is usually simpler. To expose it with zero inbound ports (and optional SSO), see [`docs/behind-cloudflare.md`](docs/behind-cloudflare.md) for a Cloudflare Tunnel + Access runbook; the client `-H` flag carries Access service tokens.

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
