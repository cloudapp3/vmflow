# Changelog

All notable user-facing changes to `vmflow` are documented here.

## Unreleased

### Added

- Unified `vmflow` binary with `daemon`, `ctl`, `tui`, and `version` subcommands.
- TCP, UDP, and `tcp+udp` forwarding rules.
- Rule lifecycle management with full snapshot apply and reload support.
- Local control API for health, rules, stats, precheck, reload, and metrics.
- Bearer-token auth with viewer/admin roles.
- Optional TLS and mutual TLS on the control API via `control_tls` (cert/key, optional `client_ca_file` for mTLS, `min_version`). Matching client flags `-tls-ca-file` / `-tls-client-cert` / `-tls-client-key` / `-tls-skip-verify` (and `VMFLOW_TLS_*` env). mTLS satisfies the non-loopback fail-closed rule.
- Repeatable client header flag `-H` / `--header` (and `VMFLOW_HEADERS` env, semicolon-separated) on `vmflow ctl`/`tui`, `relayctl`, `relaytui` — lets clients send arbitrary headers such as Cloudflare Access service tokens (`CF-Access-Client-Id` / `CF-Access-Client-Secret`).
- New runbook `docs/behind-cloudflare.md`: expose the control API behind Cloudflare Tunnel + Access (free tier) with zero inbound ports.
- Structured text/JSON logging.
- Prometheus-compatible `/metrics` endpoint.
- Rule precheck for loops, duplicate listeners, and unavailable ports.
- Embeddable top-level Go runtime API.
- GitHub Actions CI and GoReleaser release configuration.

### Changed

- Public documentation now recommends the single `vmflow` binary instead of separate `relayd`, `relayctl`, and `relaytui` artifacts.

### Security

- **Breaking:** the daemon now refuses to start when the control API is bound to a non-loopback address (`0.0.0.0`, `::`, a non-loopback IP, or `:port`) without `auth.enabled`. Previously this only logged a warning, leaving an unauthenticated remote admin endpoint open to anyone who could reach the port. Bind to `127.0.0.1`, enable `auth`, or pass `--insecure-allow-remote-control` to opt back in.
- TCP forwarding now bounds connection lifetime: each rule has an idle timeout (per-rule `idle_timeout` in seconds, default 5 min when 0), and `Stop` force-closes established connections. A silent/held-open client can no longer wedge rule stop, config reload, or daemon shutdown (previously an unbounded hang exploitable by any client that could reach a forwarding port). Backed by new `engine` tests.
- Self-update (`AtomicReplace`) now uses a random, exclusive temp file name (`os.CreateTemp`) instead of a fixed `.vmflow-update-tmp`, so a local attacker who can write to the install directory can no longer pre-plant that path as a symlink and have the update overwrite an arbitrary file through it. Defense-in-depth `Lstat` check added before rename.
- Control API auth failures are now throttled per peer IP (HTTP 429 after 10 failures within 1 minute) to slow online brute-forcing of bearer tokens.
- `install.sh` extracts release archives with `--no-same-owner`, so a malicious archive cannot restore attacker-controlled uid/gid when installed via `curl|sudo bash`.
- Control API error responses and `config_path` no longer echo internal filesystem paths (load/reload failures return a generic message with the detail kept in daemon logs; `config_path` returns the file's base name only).

### Notes

- Historical stats, web dashboard, graceful drain, and systemd/Docker packaging remain roadmap items.
