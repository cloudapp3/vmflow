# vmflow 功能清单

`vmflow` 是一个纯 Go 的 **L4/L7 转发运行时**:既可作为独立守护进程运行,也可作为库嵌入更大的控制面。核心只负责"进程内转发 + 规则生命周期 + 实时计数",持久化、鉴权、UI、审计等交给宿主。

> 本文档反映主分支当前状态。两块能力目前**暂未启用**:
> - NAT 隧道(`tunnel-server` / `tunnel-client` / `tunnel-ctl`),见文末 [NAT 隧道](#nat-隧道暂未启用)。
> - HTTP/HTTPS 转发与 ACME/证书管理(`protocol: http|https`、`/v1/certs*`、`certs` 子命令),源码保留在 `acme/`、`certstore/`、`certreview/`、`engine/https.go`、`engine/proxy.go`。

---

## 1. 转发引擎(`engine/`)

当前启用 3 种协议(tcp / udp / tcp+udp);`http`、`https` 暂未启用:

| 协议 | 能力 |
|---|---|
| `tcp` | TCP 端口转发 |
| `udp` | UDP 端口转发 |
| `tcp+udp` | 同端口同时转发 TCP 与 UDP |
| `http` | **HTTP 正向代理**:处理普通 HTTP 代理请求 + `CONNECT` 隧道(HTTPS) _(暂未启用)_ |
| `https` | **TLS 终结转发**,按 `domains` 自动申请证书后转发到目标 _(暂未启用,随 ACME 一并屏蔽)_ |

**每条规则的运行期能力**(`Rule` 字段):

- `speed_limit` —— 每连接双向速率限制(令牌桶)
- `max_conn` —— 并发连接上限,超限直接关闭新连接
- `idle_timeout` —— 每连接空闲超时(秒,默认 300);静默连接到期回收,且 `Stop` 会强制关闭已建立连接,避免粘连接卡死"停规则 / reload / 关停"
- `domains` —— HTTPS 规则的域名(用于 ACME / SNI)
- `enabled` / `remark` / `revision` / `created_time` / `updated_time`
- 规则校验、标准化,以及 `RuntimeEqual`(只比较影响运行的字段,用于增量 diff)

## 2. 规则生命周期(`engine/Manager` + 顶层 `runtime.go`)

- 单条:`StartRule` / `RestartRule` / `StopRule` / `RemoveRule`
- 批量:`ApplySnapshot(rules, {ReplaceAll})` —— 增量 diff,产出每条规则的动作(`started` / `restarted` / `stopped` / `removed` / `unchanged` / `failed`)及汇总(`Applied / Stopped / Failed / Total`)
- 状态查询:`RunningRules` / `RunningCount` / `Snapshot(id)` / `SnapshotAll()` / `StopAll()`

## 3. 可嵌入运行时(`runtime.go`)

库形态 API,供宿主进程内调用:`New()` / `NewRuntime(opts)`,以及 `Apply` / `ApplySnapshot` / `Start/Restart/Stop/RemoveRule` / `SnapshotAll` / `Close` / `Shutdown(ctx)`,并暴露 `Manager()` 与 `Collector()`。详见 [`EMBEDDING.md`](EMBEDDING.md)。

## 4. 配置体系(`config/`)

YAML 单文件,字段:

- `version` / `control_listen_addr`
- `log`:`level` + `format`(text / json)
- `auth`:`enabled` + `tokens[]`(`name` / `token` / `role`)
- `bot_token` / `bot_chat`(Telegram)
- ACME:`acme_challenge`(http-01 / dns-01)、`acme_http01_addr`、`acme_cache_dir`、`acme_dns01`(`provider` / `propagation_timeout` / `polling_interval` / `cloudflare_api_token` / `rfc2136_*` / `exec_path`)_(暂未启用,配置项保留但被忽略)_
- `cert_cache_dir`、`cert_review`(`expiry_warning_days` / `expiry_critical_days` / `min_rsa_bits`)_(暂未启用,配置项保留但被忽略)_
- `rules[]`

支持**热重载**:`POST /v1/reload` 重新读取配置并执行 `ApplySnapshot`。

## 5. Control API(`controlapi/`)

本地 HTTP 控制面,端点:

- `GET /healthz`
- `GET /v1/rules`、`GET /v1/stats`
- `POST /v1/precheck`、`POST /v1/reload`
- `GET /metrics`(Prometheus)
- ~~`GET /v1/certs`、`GET /v1/certs/<domain>`、`POST /v1/certs/obtain`、`GET /v1/certs/review`~~ _(暂未启用,路由未注册)_

**鉴权**:Bearer Token;角色 `admin` / `viewer`;常量时间比较;auth 关闭时视为可信匿名(默认建议绑 `127.0.0.1`,非回环绑定会告警)。详见 [`API.md`](API.md)。

## 6. 证书与 ACME(`acme/` + `certstore/`)_(暂未启用)_

- **HTTP-01**:内置 challenge server(`acme_http01_addr`)
- **DNS-01**:provider 可选 `cloudflare` / `exec`(外部脚本)/ `rfc2136`(配置项已就绪)
- Let's Encrypt 目录、账户注册、授权、CSR、磁盘缓存
- HTTPS 规则启动时**按域名自动申请**;`RenewLoop` 每日检查、30 天内自动续期;`Preload` 启动时从磁盘载入
- `certstore` 统一管理 ACME 证书与**手动导入 PEM**(`Import`),提供 `List` / `Get` / `Delete` / `RefreshACME`,并按 SNI(`GetCertificate`)分发给 HTTPS runner

## 7. 证书巡检(`certreview/`)_(暂未启用)_

`GET /v1/certs/review` 产出按严重度(critical / warning / info)分类的诊断:

- 证书过期(按 warning / critical 天数)
- 域名不匹配、SAN 不匹配
- 孤儿证书(有证书但无对应规则)
- 密钥强度(低于 `min_rsa_bits`)

## 8. Telegram Bot(`bot/`)

配置 `bot_token` + `bot_chat` 后启动,命令:

- `/rules` 列出规则
- `/reload` 重载配置(带确认)
- `/stop <id>` 停止单条(带确认)
- `/stopall` 全停

## 9. TUI(`tui/`)

终端仪表盘,通过 Control API 拉取数据;`[tab]` 在 Dashboard → Rules → Detail 视图间切换。

## 10. 指标(`metrics/`)

Prometheus 文本格式,指标族:

- `vmflow_build_info`、`vmflow_uptime_seconds`
- `vmflow_rule_running`、`vmflow_rule_connections`
- `vmflow_rule_upload_bytes_total`、`vmflow_rule_download_bytes_total`
- `vmflow_control_requests_total`、`vmflow_control_request_duration_seconds`
- `vmflow_reload_total`、`vmflow_rule_apply_total`

## 11. 预检(`precheck/`)

应用规则前的静态 / 动态检查,产出 error / warning:

- `duplicate_rule_id`(快照内重复 ID)
- 监听端点冲突(重复 `listen_addr:port`)
- **端口可绑定探测**(实际尝试 bind)
- HTTPS 域名配置检查
- 目标地址 DNS 解析检查
- ACME 配置检查

## 12. 日志(`internal/logging`)

结构化日志(slog),`level` + `format`(text / json),全局 `slog.SetDefault`;各组件带 `component` / `event` 字段。

## 13. CLI(`cmd/vmflow`)

单二进制:

- `vmflow daemon [-config] [-control-listen]`
- `vmflow ctl ... <health|rules|stats|metrics|precheck|reload|certs|certs-obtain|certs-review>`
- `vmflow tui`、`vmflow version [-json]`
- 别名:`d / c / t / v`
- 旧入口 `cmd/relayd` / `relayctl` / `relaytui` 仍可编译(兼容保留)

## 14. 构建 / 发布 / CI

- GoReleaser:跨平台 archive、`.deb` / `.rpm`、`checksums.txt`
- `install.sh`:下载 release、SHA-256 校验、自动选择安装目录,支持 `--version` / `--dir` / `--skip-verify` / `GITHUB_TOKEN`
- GitHub Actions CI;`Makefile`:`fmt / test / vet / smoke / build`

## NAT 隧道(暂未启用)

`tunnel-server` / `tunnel-client` / `tunnel-ctl`(TCP/UDP NAT 中继穿透)在主分支当前构建中**已屏蔽**:

- `cmd/vmflow/main.go` 移除了接线,二进制不再包含隧道命令与 `tunnel/` 包代码
- `tunnel/` 源码完整保留;文档与示例移至 `disabled/`
- 恢复方式:revert `main.go` 中那处改动,并把 `disabled/` 下的 3 个文件移回原位

## 目录速查

| 目录 | 职责 |
|---|---|
| `engine/` | 转发引擎、协议 runner、流量统计 |
| `runtime.go` | 可嵌入运行时 API |
| `config/` | YAML 加载与校验 |
| `controlapi/` | 控制 API、鉴权、reload、precheck、metrics 接线 |
| `acme/` `certstore/` `certreview/` | 证书申请 / 存储 / 巡检 |
| `bot/` | Telegram bot |
| `tui/` | 终端仪表盘 |
| `metrics/` `precheck/` | Prometheus 指标 / 规则预检 |
| `internal/logging/` | 结构化日志 |
| `cmd/vmflow/` | 主二进制(及旧 `relay*` 兼容入口) |
| `tunnel/` | NAT 隧道(暂未启用,源码保留) |
