# Embedding vmflow

`vmflow` can run as a standalone daemon, but the core forwarding runtime is also
intended to be embedded into a larger control plane such as `vmpulse`.

## Responsibility split

When embedded, keep responsibilities separated:

| Layer | Responsibility |
| --- | --- |
| host application, e.g. `vmpulse` | database, users, billing, Web UI, node orchestration, rule ownership, historical aggregation |
| `vmflow` runtime | TCP/UDP forwarding, rule lifecycle, max connection enforcement, real-time counters _(HTTP/HTTPS forwarding 暂未启用)_ |

The dependency direction should stay one-way:

```text
vmpulse -> github.com/cloudapp3/vmflow
```

`vmflow` should not import or depend on `vmpulse` models, database code, or task
protocols.

## Recommended API

For most embedders, use the top-level facade:

```go
package main

import (
    "github.com/cloudapp3/vmflow"
    "github.com/cloudapp3/vmflow/engine"
)

func main() {
    rt := vmflow.New()
    defer rt.Close()

    rules := []engine.Rule{
        {
            RuleID:     "ssh-forward",
            Name:       "ssh-forward",
            Protocol:   engine.ProtocolTCP,
            ListenAddr: "0.0.0.0",
            ListenPort: 2201,
            TargetAddr: "127.0.0.1",
            TargetPort: 22,
            Enabled:    true,
        },
    }

    result := rt.Apply(rules) // full replacement snapshot
    if result.FailedRules > 0 {
        // handle failed rules in host application
    }

    snapshots := rt.SnapshotAll()
    _ = snapshots
}
```

The lower-level `engine.Manager` API remains available for advanced use:

```go
manager := rt.Manager()
```

Prefer `Runtime.Apply` when the host application computes the complete desired
state from its own database. Use `StartRule`, `RestartRule`, `StopRule`, and
`RemoveRule` only for targeted local operations.

## Data flow for vmpulse-style embedding

Recommended flow:

```text
vmpulse DB / business API
        ↓
convert business forwarding records to []engine.Rule
        ↓
vmflow.Runtime.Apply(rules)
        ↓
vmflow.Runtime.SnapshotAll()
        ↓
vmpulse samples and persists traffic history in its own DB
```

In embedded mode, avoid making YAML the source of truth. The host application
should own desired state and pass `[]engine.Rule` directly to `vmflow`.

## Persistence guidance

`vmflow` may provide a local SQLite store for standalone daemon mode. When
embedded into `vmpulse`, prefer one of these options:

1. Let `vmpulse` persist traffic history and audit logs in its existing database.
2. Disable any local `vmflow` store.
3. Use `vmflow` only for real-time counters and forwarding lifecycle.

This avoids two competing sources of truth.

## HTTPS rules and certificates(暂未启用)

> HTTPS 转发与 ACME/证书管理在当前构建中**暂未启用**:`engine` 的 `rule.Validate()` 拒绝 `http`/`https` 协议,daemon 不再启动 ACME,`/v1/certs*` 路由与 `certs` CLI 子命令已移除。下方接口仅供重新启用时参考;源码保留在 `acme/`、`certstore/`、`certreview/`、`engine/https.go`、`engine/proxy.go`。

HTTPS rules require a certificate provider. Standalone daemon mode can use the
built-in ACME manager. Embedded applications can inject their own provider:

```go
rt := vmflow.NewRuntime(vmflow.Options{
    CertProvider: myProvider,
})
```

The provider must satisfy:

```go
type CertProvider interface {
    GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error)
    Obtain(ctx context.Context, domains []string) error
}
```

## Stable vs optional packages

Stable embedding surface:

```text
github.com/cloudapp3/vmflow
github.com/cloudapp3/vmflow/engine
```

Optional daemon/control packages:

```text
config/
controlapi/
tui/
bot/
acme/
cmd/
```

The optional packages are useful for standalone `vmflow` deployments but should
not be required by an embedded `vmpulse` integration.

## Shutdown

Use `Close` or `Shutdown` when the host application exits:

```go
_ = rt.Shutdown(ctx)
```

The current implementation stops synchronously. The `Shutdown(ctx)` shape is
reserved for future graceful drain support.
