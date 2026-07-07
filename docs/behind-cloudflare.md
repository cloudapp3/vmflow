# Exposing the control API behind Cloudflare (Tunnel + Access)

This is a zero-inbound-port way to reach the vmflow control API over the public
internet with free-tier Cloudflare: TLS, DDoS protection, hidden origin, and
(optionally) SSO/service-token authentication. No port needs to be opened on
the host running vmflow.

## What you get

- **TLS**: Cloudflare issues the public certificate automatically (no Let's
  Encrypt or self-signed setup).
- **Hidden origin**: the daemon stays bound to `127.0.0.1`; `cloudflared`
  dials out to Cloudflare, so nothing listens on a public interface.
- **DDoS / WAF / rate limiting / IP allowlist**: basic tiers are free.
- **Identity** (optional): Cloudflare Access (Zero Trust, free up to 50 users)
  gates who can reach the API — humans via SSO/email OTP/GitHub/Google,
  machines (e.g. `vmflow ctl`) via **service tokens**.

> Cloudflare's free tier does **not** support client-certificate mTLS. For
> machine identity behind Cloudflare, use Access service tokens (`-H`), not
> `control_tls.client_ca_file`.

## Architecture

```
vmflow daemon (127.0.0.1:19090)  ←─ cloudflared (outbound) ─→  Cloudflare edge
                                                               (HTTPS + Access + WAF)
                                                                    │
                                                                    ▼
                                                       ops machine / CI (`vmflow ctl`)
```

Because the daemon is on loopback, the fail-closed startup check passes with no
extra flags.

---

## Option A — Tunnel + vmflow bearer auth (no code, recommended start)

1. Daemon config — bind loopback, enable bearer auth:

   ```yaml
   control_listen_addr: 127.0.0.1:19090
   auth:
     enabled: true
     tokens:
       - name: admin
         token: <long-random-bearer-token>
         role: admin
   ```

2. Install `cloudflared` and create a named tunnel:

   ```bash
   cloudflared tunnel login            # authorizes your Cloudflare account
   cloudflared tunnel create vmflow
   cloudflared tunnel route dns vmflow ctrl.example.com
   ```

   ```yaml
   # ~/.cloudflared/config.yml
   tunnel: <TUNNEL_ID>
   credentials-file: /root/.cloudflared/<TUNNEL_ID>.json
   ingress:
     - hostname: ctrl.example.com
       service: http://localhost:19090
     - service: http_status:404
   ```

3. Run it (systemd unit):

   ```ini
   # /etc/systemd/system/cloudflared.service
   [Unit]
   Description=cloudflared tunnel for vmflow control API
   After=network.target

   [Service]
   ExecStart=/usr/bin/cloudflared tunnel --config /root/.cloudflared/config.yml run vmflow
   Restart=on-failure
   User=cloudflared

   [Install]
   WantedBy=multi-user.target
   ```

   ```bash
   systemctl enable --now cloudflared
   ```

4. (Recommended) In the Cloudflare dashboard add a WAF custom rule / IP
   allowlist on `ctrl.example.com` so only your ops egress IPs can reach it,
   plus a rate-limit rule.

5. Client — works with existing flags, no `-H` needed:

   ```bash
   vmflow ctl -addr https://ctrl.example.com -token <bearer> health
   ```

You now have: free TLS, hidden origin, Cloudflare DDoS/rate-limit, and vmflow
bearer auth. For most setups this is sufficient.

---

## Option B — add Cloudflare Access (service token) on top

This moves "who can call the API" to Cloudflare Access, so you get a unified
identity/audit layer (and can drop the vmflow bearer if you prefer).

1. Cloudflare Zero Trust → **Access → Applications → Add application**
   (Self-hosted), application domain `ctrl.example.com`. Add a policy
   (e.g. allow your team's emails, or a service-token identity).

2. **Access → Service Tokens → Create**. Note the **Client ID** and
   **Client Secret**.

3. Machine client (`vmflow ctl` / `relayctl`) uses the new `-H` / `--header`
   flag (repeatable) to send the service-token headers:

   ```bash
   vmflow ctl -addr https://ctrl.example.com \
     -H "CF-Access-Client-Id: $CF_ID" \
     -H "CF-Access-Client-Secret: $CF_SECRET" \
     -token <bearer> health
   ```

   Or via environment (semicolon-separated, avoids putting secrets on the
   command line / in shell history):

   ```bash
   export VMFLOW_HEADERS="CF-Access-Client-Id: $CF_ID; CF-Access-Client-Secret: $CF_SECRET"
   vmflow ctl -addr https://ctrl.example.com -token <bearer> health
   ```

   `-H`/`--header` accepts `Name: Value` or `Name=Value`, and applies to
   `vmflow ctl`, `vmflow tui`, `relayctl`, and `relaytui`. The TUI also honors
   it on every poll request.

4. Keep vmflow `auth.enabled: true` as defense-in-depth, or turn it off and
   rely solely on Access (less recommended — no second factor at the origin).

---

## Option C (defense-in-depth, not built here) — verify Access JWT at the daemon

Cloudflare injects `CF-Access-Jwt-Assertion` on every request that passes
Access (including service-token ones). You can add a small middleware in
`controlapi` that verifies that JWT with Cloudflare's JWKS
(`https://<TEAM>.cloudflareaccess.com/cdn-cgi/access/certs`) so the daemon
rejects any request that didn't actually traverse Access. This is ~80–120
lines and only worth it for high-compliance environments; not currently
implemented.

---

## Limitations / notes

- **Client-certificate mTLS is not available** on Cloudflare's free tier —
  use Access service tokens (`-H`) for machine identity.
- **Certificate rotation** behind the tunnel is invisible to vmflow (Cloudflare
  manages the public cert). If you ever set `control_tls` on the daemon too,
  note that swapping those files requires a daemon restart (`/v1/reload` does
  not rebuild TLS).
- **Cloudflare request timeout** is ~100s per request. `vmflow tui` does short
  polling, so it is unaffected; avoid long-lived streaming calls through CF.
- **Network reachability**: Cloudflare can be slow or blocked in some regions;
  assess for your ops network.
- The daemon on loopback with `auth.enabled: false` behind a tunnel is safe
  only if you fully trust Access (Option C) — otherwise keep bearer auth on.

## Security checklist

- [ ] Daemon bound to `127.0.0.1` (no public listen).
- [ ] `auth.enabled: true` with a high-entropy token (unless using Option C).
- [ ] `cloudflared` runs as a non-root user and restarts on failure.
- [ ] Cloudflare WAF: IP allowlist + rate-limit on `ctrl.example.com`.
- [ ] (Option B) Service-token secret stored in a secret manager / env, not git.
- [ ] (Option C, if used) JWKS refresh on a schedule.
