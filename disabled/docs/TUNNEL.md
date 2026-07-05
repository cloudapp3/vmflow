# vmflow tunnel

`vmflow tunnel` adds a lightweight NAT traversal mode on top of the existing
L4 forwarding runtime.

It is intentionally not a full `frp`/`gost` clone. The first implementation is a
small, observable TCP tunnel suitable for embedding into a larger control plane.

## Topology

```text
public user
  -> tunnel-server remote port on a public VPS
  -> vmflow tunnel control/data connections
  -> tunnel-client inside NAT
  -> local service such as 127.0.0.1:22
```

The tunnel client always dials out to the tunnel server, so it works behind NAT
as long as outbound TCP to the server is allowed.

## Commands

```bash
vmflow tunnel-server -config ./examples/tunnel-server.yaml
vmflow tunnel-client -config ./examples/tunnel-client.yaml
```

Aliases:

```bash
vmflow ts -config ./examples/tunnel-server.yaml
vmflow tc -config ./examples/tunnel-client.yaml
vmflow tctl health
vmflow tctl tunnels
```

## Admin API and metrics

`tunnel-server` starts a local admin API on `admin_listen_addr`, defaulting to
`127.0.0.1:19091`.

Endpoints:

- `GET /healthz`
- `GET /v1/tunnel/clients`
- `GET /v1/tunnel/tunnels`
- `GET /v1/tunnel/stats`
- `GET /metrics`
- `GET|POST /v1/tunnel/precheck`
- `POST /v1/tunnel/reload`

Example:

```bash
vmflow tunnel-ctl health
vmflow tunnel-ctl clients
vmflow tunnel-ctl tunnels
vmflow tunnel-ctl stats
vmflow tunnel-ctl metrics
vmflow tunnel-ctl precheck
vmflow tunnel-ctl reload

# Equivalent curl form:
curl http://127.0.0.1:19091/healthz
curl http://127.0.0.1:19091/v1/tunnel/tunnels
curl http://127.0.0.1:19091/metrics
```

The same Bearer-token auth shape used by the normal daemon Admin API can be
enabled in the tunnel-server config under `auth`. `reload` requires an admin
role token; viewer tokens can read health/state/metrics only.

Reload currently hot-applies client ACL/auth/open-timeout style changes. Existing
clients that no longer match the new ACL or token are disconnected and can
reconnect if allowed. Listener address and TLS certificate changes are reported
as warnings by `precheck` and require a process restart.

## MVP transport

The MVP uses:

- one long-lived control TCP connection from client to server;
- one short-lived data TCP connection per accepted public TCP connection;
- JSON-line control messages;
- TCP and UDP tunnels;
- `client_id` + token authentication;
- server-side ACL checks for protocol, remote ports, and tunnel count;
- local admin API for health, clients, tunnels, stats, and metrics;
- optional TLS on the tunnel server listener.

This keeps the first version dependency-light and easy to audit. A later version
can replace the per-connection data socket with yamux or QUIC while preserving
the config shape.

## Local SSH example

On the public server:

```bash
vmflow tunnel-server -config ./examples/tunnel-server.yaml
```

On the NAT/private machine:

```bash
vmflow tunnel-client -config ./examples/tunnel-client.yaml
```

Then from outside:

```bash
ssh -p 2201 user@PUBLIC_SERVER_IP
```

The connection reaches `127.0.0.1:22` on the private machine.

## UDP example

Add a UDP tunnel in the client config:

```yaml
- tunnel_id: home-dns
  protocol: udp
  remote_listen_addr: 0.0.0.0
  remote_listen_port: 5353
  local_addr: 127.0.0.1
  local_port: 53
```

Allow the remote port on the server side:

```yaml
allow:
  protocols: ["tcp", "udp"]
  remote_ports: [2201, 8080, 5353]
```

Then query DNS through the public server port:

```bash
dig @PUBLIC_SERVER_IP -p 5353 example.com
```

## Security notes

- Do not expose the tunnel server on the Internet without a long random token.
- Prefer enabling `tunnel_server.tls` or putting the listener behind a TLS
  terminator for Internet use.
- Restrict every client with `allow.remote_ports` and `allow.protocols`.
- UDP is supported through per-remote-address sessions over tunnel data connections.
- HTTP/SNI routing is a future extension.
