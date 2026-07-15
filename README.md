# vmflow - Go TCP/UDP Port Forwarding

**A bounded-resource Layer 4 (L4) proxy and embeddable Go networking runtime.**

vmflow is a self-hosted, cross-platform TCP/UDP port forwarding tool written in
pure Go. Run it as a single-binary network proxy or embed the forwarding runtime
in your own Go control plane, with explicit resource limits, hot-reloadable
rules, a terminal UI, and Prometheus metrics.

[![Docs](https://img.shields.io/badge/docs-source-14b8a6)](https://github.com/cloudapp3/vmdocs/tree/main/sites/vmflow/docs)
[![CI](https://github.com/cloudapp3/vmflow/actions/workflows/go.yml/badge.svg)](https://github.com/cloudapp3/vmflow/actions/workflows/go.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/cloudapp3/vmflow.svg)](https://pkg.go.dev/github.com/cloudapp3/vmflow)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Documentation: [English](https://github.com/cloudapp3/vmdocs/blob/main/sites/vmflow/docs/index.md) · [中文说明](https://github.com/cloudapp3/vmdocs/blob/main/sites/vmflow/docs/zh/index.md) · [HTTP API](https://github.com/cloudapp3/vmdocs/blob/main/sites/vmflow/docs/api.md) · [Docs source](https://github.com/cloudapp3/vmdocs)

> **完整使用指南:** [中文文档源码](https://github.com/cloudapp3/vmdocs/tree/main/sites/vmflow/docs/zh) —— 覆盖安装、配置、运维、远程访问(TLS/mTLS/Cloudflare)、安全加固与排错。English quick reference is below; the deep guide is in the [public docs source](https://github.com/cloudapp3/vmdocs/tree/main/sites/vmflow/docs).

## TCP/UDP port forwarding features

- TCP, UDP, and `tcp+udp` port forwarding
- Configurable TCP connection limits and bounded UDP sessions, with rejection/drop counters
- Rule lifecycle management: start, stop, restart, and full snapshot apply
- Config-driven foreground process with hot reload
- Local control API for rules, stats, precheck, reload, and metrics
- Bearer-token auth with viewer/admin roles
- Structured logs in text or JSON format
- Prometheus-compatible `/metrics`
- Optional durable per-rule cumulative traffic and drop counters
- Rule precheck for loops, duplicate ports, and unavailable listeners
- Embeddable Go runtime for products that need in-process forwarding
- Terminal dashboard and rule management via `vmflow tui`

## Common use cases

- Forward TCP services such as SSH, databases, and internal APIs between hosts.
- Relay UDP services with bounded sessions, queues, idle cleanup, and drop metrics.
- Run a self-hosted Layer 4 proxy on Linux, macOS, or Windows as a native service.
- Embed TCP/UDP forwarding into a Go application, VPS panel, or network control plane.

## Quick start

Install the latest prebuilt binary and a colocated config (Linux/macOS):

```bash
curl -fsSL https://raw.githubusercontent.com/cloudapp3/vmflow/main/install.sh \
  | bash -s -- --dir "$HOME/.local/bin"
```

The first install creates `~/.local/bin/config.yaml` with mode `0600`. Reinstalling
or upgrading replaces only the binary and preserves the existing config. Running
`vmflow` uses the `config.yaml` beside the resolved binary by default; `-config`
overrides that path.

The bundled SSH forwarding example is disabled and listens on loopback only.
Review its listen address, target, and firewall exposure before setting
`enabled: true`; a first launch starts the control plane without exposing a new
forwarding port.

Start vmflow in one terminal:

```bash
"$HOME/.local/bin/vmflow"
```

Query it from another terminal:

```bash
"$HOME/.local/bin/vmflow" ctl rules
"$HOME/.local/bin/vmflow" ctl stats
"$HOME/.local/bin/vmflow" ctl metrics
"$HOME/.local/bin/vmflow" ctl precheck
```

Open the terminal UI or show build metadata:

```bash
"$HOME/.local/bin/vmflow" tui
"$HOME/.local/bin/vmflow" version -json
```

For a root-owned system installation, put both files in `/usr/local/bin`:

```bash
curl -fsSL https://raw.githubusercontent.com/cloudapp3/vmflow/main/install.sh | sudo bash -s -- --dir /usr/local/bin
```

The installer downloads GitHub Release archives, verifies `checksums.txt` with SHA-256 by default, and auto-detects an install directory (`/usr/local/bin` → `~/.local/bin` → `~/bin`) when `--dir` is omitted. It installs `vmflow` and, only when absent, `config.yaml` in that directory. You can override the directory with `--dir PATH` or `VMFLOW_INSTALL_DIR`, and skip checksum verification with `--skip-verify` if needed. For private releases or higher GitHub API limits, set `GITHUB_TOKEN` or `GH_TOKEN`.

Or build from source:

```bash
git clone https://github.com/cloudapp3/vmflow.git
cd vmflow
go build -trimpath -o vmflow ./cmd/vmflow
cp -n examples/config.yaml config.yaml
./vmflow
```

## Configuration

See [`examples/config.yaml`](examples/config.yaml):

```yaml
version: 1
control_listen_addr: 127.0.0.1:19090
udp_max_sessions: 256              # all UDP sessions in this process
# control_tls:                       # enable TLS on the control API
#   cert_file: /etc/vmflow/control.crt
#   key_file: /etc/vmflow/control.key
#   client_ca_file: clients-ca.crt   # optional: require client certs (mTLS)
#   min_version: "1.2"               # "1.2" (default) | "1.3"

log:
  level: info
  format: text

stats:
  persist: false                    # opt in to cumulative counter persistence
  # path: /var/lib/vmflow/stats.json # optional; relative paths use the config dir
  flush_interval: 60s               # minimum 1s; requires restart when changed

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
    listen_addr: 127.0.0.1
    listen_port: 2201
    target_addr: 127.0.0.1
    target_port: 22
    enabled: false
    max_conn: 0                     # TCP: unlimited; UDP: default of 256
```

`udp_max_sessions` limits active UDP sessions across every rule owned by the
process (default `256`, maximum `4096`) and can be changed by config reload.
Lowering it below current usage does not terminate established sessions; it
rejects new UDP sessions until usage falls below the new limit. For each UDP
rule, `max_conn: 0` uses the default of `256`; for TCP, `max_conn: 0` remains
unlimited. On a `tcp+udp` rule, `max_conn` is enforced independently for TCP
connections and UDP sessions; UDP sessions also consume the process-wide limit.
Each UDP session owns a socket, a receive goroutine, and a 64 KiB receive
buffer. Check available memory and open-file limits before raising either cap.

Set `stats.persist: true` to preserve per-rule upload/download byte totals and
UDP rejection/drop counters across restarts. Active connection counts and rates
remain process-local. The foreground default is `stats.json` beside the loaded
config. The installed Linux systemd unit uses its managed state directory
(`/var/lib/vmflow/stats.json`), including when running with `--user vmflow`.
An explicit relative `stats.path` is resolved beside the config file. vmflow
refuses to start if the stats path aliases the config file or cannot be written.

Hot reload applies only `rules` and `udp_max_sessions`. Changes to the control
listen address, auth, TLS, logging, bot, ACME, certificate cache, or certificate
review or stats persistence settings return HTTP `409` and require a vmflow
process restart; they are never reported as successfully applied while the old
runtime settings remain active.

Security note: vmflow **refuses to start** if the control API is bound to a non-loopback address without auth. Keep it on `127.0.0.1` (the default), or enable `auth` before exposing it. To bind remotely without auth anyway, pass `--insecure-allow-remote-control` (not recommended — put it behind a TLS-terminating reverse proxy instead).

### TUI rule management

`vmflow tui -token ADMIN_TOKEN` can manage `rules` and `udp_max_sessions` when
auth is enabled and the token has the `admin` role. Viewer tokens and sessions
with auth disabled are read-only. In the Rules view use `n`/`e`/`c` to create,
edit, or copy, `space` to toggle, `d` to delete, `g` for the global UDP limit,
`P` to precheck, `A` to apply, and `u` to discard the draft.

Apply writes the validated draft to the config loaded by the running process,
which is the `config.yaml` beside the resolved vmflow binary by default (or the
explicit `-config` path). Auth, TLS, logging, and other process settings still
require editing YAML and restarting vmflow.

## Commands

```bash
vmflow               [-config path] [-control-listen 127.0.0.1:19090] [-insecure-allow-remote-control]
vmflow ctl           [-addr http://127.0.0.1:19090] [-token TOKEN] <rules|stats|metrics|precheck|reload>
vmflow tui           [-addr http://127.0.0.1:19090] [-token TOKEN]
vmflow version       [-json]
vmflow update        [--check] [--version tag]
vmflow service       (install|uninstall|status) [--config path] [--binary path] [--user name] [--log-file path]
                     [--control-listen addr] [--insecure-allow-remote-control] [--extra-arg value]...
vmflow uninstall     [--dry-run]
```

Self-update is supported on Linux and macOS. On Windows, download and install
the release ZIP manually.

Aliases are available: `ctl=c`, `tui=t`, `version=v`, `update=u`, and `service=svc`.

## Run as a service (boot startup)

Register vmflow as a native OS service so it starts at boot and restarts on crash — one command on every platform:

```bash
sudo vmflow service install --config /usr/local/bin/config.yaml
```

For safety, `service install` refuses to register a service that points at a
relative path, a user-writable binary or config file, or either file under
user-writable parent directories (symlinks are resolved first). Install
`vmflow` into a protected root/admin-owned path such as
`/usr/local/bin/vmflow`, `/opt/vmflow/vmflow`, or
`C:\Program Files\vmflow\vmflow.exe`, and keep the service config in a protected
root/admin-owned location such as `/usr/local/bin/config.yaml` or
`/etc/vmflow/config.yaml`; pass `--binary` if you need to point at the installed
binary explicitly. A service running as a dedicated user needs explicit read
access to the root-owned config (for example, owner `root`, group `vmflow`, mode
`0640`).

The config is parsed before the OS service definition is changed. Running
`service install` again updates the existing definition and restarts the
service with the new settings.

- **Linux**: writes and reloads a systemd unit, enables it, restarts the service, and verifies it is active. Logs go to journald (`journalctl -u vmflow`). The unit runs as root by default with `CAP_NET_BIND_SERVICE` (so it can bind privileged ports), `Restart=on-failure`, and a writable `/var/lib/vmflow` state directory. Pass `--user vmflow` to run under a dedicated account (created if missing).
- **macOS**: writes a launchd daemon (`KeepAlive` restarts on crash) and bootstraps it. Logs land under `/var/log/vmflow/` (override with `--log-file`).
- **Windows**: registers a Windows Service (`start=auto`, restart-on-failure) visible in `services.msc`. Because the SCM provides no stdout, logs default to `C:\ProgramData\vmflow\logs\vmflow.log`; override with `--log-file` if needed.

Uninstall with `sudo vmflow service uninstall` (config and logs are left in place). Inspect with `vmflow service status`.

For a complete removal, run `sudo vmflow uninstall`. It prints the full plan
and requires confirmation before removing the service, binary, platform-default
config, installer-owned colocated config, persistent traffic statistics, logs,
update cache, and vmflow-owned certificate caches. Stats files are parsed and
revalidated immediately before removal; changed or unrecognized files are kept.
An unowned colocated `config.yaml` is preserved. External TLS certificate and key
files are never removed because they may be shared with other services. Custom
certificate cache directories are left in place unless the directory is
exclusively owned by vmflow and contains a `.vmflow-owned` marker; use
`--dry-run` to inspect the plan without changing the system.

## Control API

The control API is an **internal, loopback-only** interface (`127.0.0.1:19090`) used by the local CLI/TUI (`vmflow ctl`, `vmflow tui`) — not a public/external API. Interact via the CLI/TUI; the HTTP endpoints below are reference for local tooling and integrations. Documented in the [HTTP API guide](https://github.com/cloudapp3/vmdocs/blob/main/sites/vmflow/docs/api.md). Main endpoints:

- `GET /v1/rules`
- `GET /v1/stats`
- `GET /v1/session`
- `GET /v1/config/rules`
- `POST /v1/config/rules/precheck`
- `PUT /v1/config/rules` (admin token and `If-Match` required)
- `GET|PUT /v1/config/bot` (admin token; PUT requires `If-Match`)
- `POST /v1/bot/start`, `POST /v1/bot/stop` (admin token)
- `GET|POST /v1/precheck`
- `POST /v1/reload`
- `GET /metrics`

UDP admission-attempt failures and rate-limit queue-capacity drops are exposed in rule stats and
as `vmflow_udp_session_rejected_total` and
`vmflow_udp_packets_dropped_total` metrics. Manager-wide usage is exposed as
`vmflow_udp_sessions_active` and `vmflow_udp_sessions_limit`.

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
vmflow ctl -addr https://host:19090 -tls-ca-file ca.crt rules
```

With `cert_file`, `key_file`, and `client_ca_file` set (mTLS), the control API counts as authenticated for the non-loopback fail-closed rule, so it can be exposed without bearer auth. For public exposure, binding loopback behind a TLS-terminating reverse proxy (Caddy/Nginx + ACME) is usually simpler. To expose it with zero inbound ports (and optional SSO), see the [deployment guide](https://github.com/cloudapp3/vmdocs/blob/main/sites/vmflow/docs/guide/deployment.md); the client `-H` flag carries Access service tokens.

## Telegram Bot

vmflow can run an optional Telegram bot for querying and controlling forwarding from a chat. Configure it in `config.yaml`:

```yaml
bot_token: "123456:ABC-DEF..."     # Telegram bot token from @BotFather
bot_chat: 123456789                 # your chat ID (from @userinfobot)
bot_control_token: "admin-xxx"      # admin auth token; lets the bot write. Omit for read-only.
```

The bot only responds to `bot_chat` (chat-ID allowlist). Private chats and groups are supported; confirmation results stay in the originating chat. Set `bot_control_token` to an admin `auth.tokens` entry to enable write commands; without it the bot is read-only. Bot control requires `auth.enabled: true`.

Commands:

- `/status`, `/rules`, `/detail <id>` — read-only queries
- `/reload` — reload configuration from disk
- `/stop <id>`, `/start_rule <id>`, `/toggle <id>` — disable / enable / toggle a rule (persists to `config.yaml` via the control API, with precheck + optimistic locking)

Write commands go through the same authenticated control handler as the TUI and `vmflow ctl`, so they get precheck, transactional apply with rollback, optimistic concurrency (`If-Match`), and audit logging. Conflicts (HTTP 412, e.g. someone edited the config concurrently) are reported to the chat with a retry hint. Bot requests stay in-process; external control API TLS and mTLS policy remains enforced for network clients without requiring a separate client certificate for the embedded bot.

### Configuring the bot from the TUI

You can also configure and control the bot at runtime from the TUI without editing `config.yaml` or restarting the daemon:

- Press `b` in the Dashboard or Rules view to open the **Bot** panel (running state + current settings; tokens are masked).
- `e` edits `bot_token` / `bot_chat` / `bot_control_token` (token fields are masked) — `Ctrl+S` verifies the Bot token with Telegram, persists to `config.yaml`, and rebuilds the bot goroutine in place.
- `s` / `x` start / stop the bot at runtime (not persisted; a daemon restart restores from config).
- `r` refreshes.

Bot configuration changes go through `GET/PUT /v1/config/bot` (admin, `If-Match` optimistic locking), so they are conflict-aware just like rule edits. A rejected token leaves the existing bot and file untouched; a file commit failure restores the previous runtime bot. Daemon restart is **not** required and forwarding is **not** interrupted.

## Embedding vmflow

Use the top-level package when vmflow is embedded into another Go service:

```go
rt := vmflow.New()
defer rt.Close()

result := rt.Apply(rules) // []engine.Rule
stats := rt.SnapshotAll()
```

The embedding application owns persistence, auth, UI, audit logs, and business rules. `vmflow` owns only in-process forwarding, rule lifecycle, and real-time counters. See the [embedding guide](https://github.com/cloudapp3/vmdocs/blob/main/sites/vmflow/docs/library/runtime.md).

## License

MIT. See [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) for bundled dependency licenses.
