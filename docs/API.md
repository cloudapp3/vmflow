# control API

默认监听地址：`127.0.0.1:19090`

## 鉴权

> **绑定安全**:daemon 默认只在回环地址(`127.0.0.1`)监听。若 `control_listen_addr` 绑到非回环地址且未开启 `auth`,daemon 会**拒绝启动**(不再仅打日志)。如需强行远端暴露,加 `--insecure-allow-remote-control`(不建议;请改用 TLS 反向代理 + 鉴权)。

Control API 支持 Bearer Token 鉴权。配置示例：

```yaml
auth:
  enabled: true
  tokens:
    - name: admin
      token: change-me
      role: admin
    - name: viewer
      token: view-only
      role: viewer
```

请求示例：

```bash
curl -H "Authorization: Bearer change-me" http://127.0.0.1:19090/v1/stats
```

CLI/TUI 可以使用：

```bash
vmflow ctl -token change-me stats
VMFLOW_CONTROL_TOKEN=change-me vmflow tui
```

角色：

- `viewer`：可读接口，例如 health/rules/stats。
- `admin`：包含 viewer 权限，并可执行 reload 等写操作。


## TLS

控制 API 默认明文 HTTP。要启用 TLS,设置 `control_tls`:

```yaml
control_tls:
  cert_file: /etc/vmflow/control.crt
  key_file:  /etc/vmflow/control.key
  client_ca_file: /etc/vmflow/clients-ca.crt   # 可选,设了即强制 mTLS
  min_version: "1.2"   # "1.2"(默认)| "1.3"
```

客户端把 `-addr` 改成 `https://`;私有/自签 CA 传 `--tls-ca-file`(或 `VMFLOW_TLS_CA_FILE`);mTLS 再传 `--tls-client-cert` / `--tls-client-key`(或 `VMFLOW_TLS_CLIENT_CERT` / `VMFLOW_TLS_CLIENT_KEY`)。示例:

```bash
vmflow ctl -addr https://host:19090 -tls-ca-file ca.crt health
```

配置了 `client_ca_file`(mTLS)后,控制 API 视为"已认证",满足非回环 fail-closed 规则,可不带 bearer 暴露在非回环地址。公网暴露时,通常更简单的做法是绑回环 + 前置 TLS 反向代理(Caddy/Nginx + ACME)。

## `GET /healthz`

返回 daemon 健康状态。

示例响应：

```json
{
  "ok": true,
  "running_rules": 1,
  "time": 1760000000
}
```

## `GET /v1/rules`

返回正在运行的规则。

示例响应：

```json
{
  "items": [
    {
      "rule_id": "ssh-forward",
      "name": "ssh-forward",
      "protocol": "tcp",
      "listen_addr": "0.0.0.0",
      "listen_port": 2201,
      "target_addr": "127.0.0.1",
      "target_port": 22,
      "enabled": true
    }
  ]
}
```

## `GET /v1/stats`

返回所有规则的内存快照统计。

示例响应：

```json
{
  "items": [
    {
      "rule_id": "ssh-forward",
      "upload_bytes": 1024,
      "download_bytes": 2048,
      "conns": 1,
      "updated_time": 1760000000
    }
  ]
}
```



## `GET|POST /v1/precheck`

加载当前配置文件，执行规则预检查，但不应用配置。`reload` 会先执行同样的预检查；如果存在 error，会拒绝 reload。

检查内容包括：

- 规则模型校验
- 重复 `rule_id`
- listen 端口冲突
- listen 地址可绑定性
- target 地址解析
- HTTPS domain 基础校验 _(暂未启用:http/https 协议已被拒绝)_
- ACME HTTP-01 地址格式与端口冲突 _(暂未启用:ACME 子系统已屏蔽)_
- 低端口权限 warning

示例：

```bash
vmflow ctl precheck
relayctl precheck
```

示例响应：

```json
{
  "config_path": "./examples/config.yaml",
  "rule_count": 1,
  "result": {
    "ok": true,
    "error_count": 0,
    "warning_count": 0,
    "checked_rules": 1,
    "checked_time_ms": 1,
    "items": []
  }
}
```

## `GET /metrics`

返回 Prometheus text exposition 格式指标。

示例：

```text
vmflow_rule_upload_bytes_total{rule_id="ssh-forward",protocol="tcp"} 1024
vmflow_rule_download_bytes_total{rule_id="ssh-forward",protocol="tcp"} 2048
vmflow_rule_connections{rule_id="ssh-forward",protocol="tcp"} 1
vmflow_control_requests_total{method="GET",path="/v1/stats",status="200"} 10
vmflow_reload_total{status="ok"} 1
vmflow_rule_apply_total{action="started",status="ok"} 1
```

当前指标包括：

- `vmflow_build_info`
- `vmflow_uptime_seconds`
- `vmflow_rule_running{rule_id,protocol}`
- `vmflow_rule_connections{rule_id,protocol}`
- `vmflow_rule_upload_bytes_total{rule_id,protocol}`
- `vmflow_rule_download_bytes_total{rule_id,protocol}`
- `vmflow_control_requests_total{method,path,status}`
- `vmflow_control_request_duration_seconds_sum{method,path,status}`
- `vmflow_reload_total{status}`
- `vmflow_rule_apply_total{action,status}`

## `POST /v1/reload`

重新加载当前配置文件，并执行一次 `ApplySnapshot(replace_all=true)`。

示例响应：

```json
{
  "config_path": "./examples/config.yaml",
  "control_listen_addr": "127.0.0.1:19090",
  "rule_count": 1,
  "result": {
    "applied_rules": 1,
    "stopped_rules": 0,
    "failed_rules": 0,
    "total_rules": 1,
    "items": []
  }
}
```
