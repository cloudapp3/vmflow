# vmflow 使用指南

面向使用者的端到端手册:从安装、配置、运行,到日常运维、远程管控、安全加固。配套文档:[API 参考](API.md) · [功能清单](FEATURES.md) · [嵌入指南](EMBEDDING.md) · [Cloudflare 暴露](behind-cloudflare.md) · [架构](ARCHITECTURE.md)。

vmflow 有两种用法:
- **独立守护进程**:跑 `vmflow daemon`,用 `vmflow ctl` / `tui` / HTTP API 管控。本文主体。
- **嵌入库**:作为 Go 库嵌进你自己的控制面,只负责"进程内转发 + 规则生命周期 + 实时计数"。见末尾[作为库嵌入](#作为库嵌入)与 [EMBEDDING.md](EMBEDDING.md)。

---

## 1. 安装

**预编译二进制(推荐)**
```bash
curl -fsSL https://raw.githubusercontent.com/cloudapp3/vmflow/main/install.sh | bash
# 全局安装到 /usr/local/bin:
curl -fsSL https://raw.githubusercontent.com/cloudapp3/vmflow/main/install.sh | sudo bash -s -- --dir /usr/local/bin
# 指定版本:
curl -fsSL https://raw.githubusercontent.com/cloudapp3/vmflow/main/install.sh | bash -s -- --version vX.Y.Z
```
安装器从 GitHub Release 下载、用 SHA-256 校验 `checksums.txt`、自动选安装目录。私有 release 或要提高 API 限额可设 `GITHUB_TOKEN`/`GH_TOKEN`;跳过校验用 `--skip-verify`(不建议)。

**系统包**:Release 里也提供 `.deb` / `.rpm`(由 GoReleaser 产出)。

**源码编译**
```bash
go build -trimpath -o vmflow ./cmd/vmflow
```

验证:`vmflow version`。

---

## 2. 五分钟上手

终端 1,起 daemon:
```bash
vmflow daemon -config ./examples/config.yaml
```
终端 2,查询:
```bash
vmflow ctl health
vmflow ctl rules
vmflow ctl stats
vmflow ctl metrics
vmflow ctl precheck
```
终端仪表盘:
```bash
vmflow tui
```

默认控制 API 监听 `127.0.0.1:19090`(只在本机回环),无需任何额外配置即可在本地用。

---

## 3. 配置文件(全字段)

单文件 YAML。示例(带注释的全字段参考见 [`examples/config.yaml`](../examples/config.yaml)):

```yaml
version: 1
control_listen_addr: 127.0.0.1:19090

# 控制面 TLS(可选)。cert/key 都设则启用 TLS;client_ca_file 设则强制 mTLS
control_tls:
  cert_file: /etc/vmflow/control.crt
  key_file:  /etc/vmflow/control.key
  client_ca_file: /etc/vmflow/clients-ca.crt   # 可选 → mTLS
  min_version: "1.2"                            # "1.2"(默认)| "1.3"

log:
  level: info      # debug | info | warn | error
  format: text     # text | json

auth:
  enabled: false
  tokens:
    - name: admin
      token: change-me
      role: admin   # admin | viewer

# 可选:Telegram bot
bot_token: ""       # 从 @BotFather 拿
bot_chat: 0         # 允许的 chat id(整数)

rules:
  - rule_id: ssh-forward
    name: ssh-forward
    protocol: tcp            # tcp | udp | tcp+udp(http/https 当前构建禁用)
    listen_addr: 0.0.0.0
    listen_port: 2201
    target_addr: 127.0.0.1
    target_port: 22
    enabled: true
    speed_limit: 0           # 每连接速率限制,字节/秒;0=不限
    max_conn: 0              # 并发连接上限;0=不限(暴露端口不建议)
    idle_timeout: 300        # 空闲超时(秒);0=默认 5 分钟
    remark: 示例
```

**字段说明**

| 顶层字段 | 作用 |
|---|---|
| `version` | 配置版本,必须 `1` |
| `control_listen_addr` | 控制 API 监听地址,默认 `127.0.0.1:19090` |
| `control_tls` | 控制面 TLS/mTLS(详见第 8 节) |
| `log` | 日志 `level` + `format` |
| `auth` | `enabled` + `tokens[]`(`name`/`token`/`role`)|
| `bot_token` / `bot_chat` | Telegram bot |
| `rules` | 转发规则数组 |

**每条规则字段**(`rule_id` 唯一;`name`/`protocol`/`listen_*`/`target_*`/`enabled` 必填,详见 [FEATURES.md](FEATURES.md)):
`rule_id`、`name`、`protocol`、`listen_addr`、`listen_port`、`target_addr`、`target_port`、`enabled`、`speed_limit`、`max_conn`、`idle_timeout`、`remark`、`revision`、`created_time`、`updated_time`、`domains`(仅 https,当前禁用)。

**校验规则**:`listen_port`/`target_port` ∈ 1–65535;`speed_limit`/`max_conn`/`idle_timeout` ≥ 0;`rule_id` 在文件内唯一;`role` ∈ {admin, viewer};`control_tls` 的 cert/key 同设或同缺、`min_version` ∈ {"1.2","1.3"}。配置加载失败会拒绝启动。

> ACME / 证书巡检 / HTTP·HTTPS 转发 / NAT 隧道相关字段在源码里保留但**当前构建已禁用**(写了不报错,但不生效)。

---

## 4. 运行 daemon

```bash
vmflow daemon -config ./examples/config.yaml [-control-listen 127.0.0.1:19090] [-insecure-allow-remote-control]
```
- `-config`:配置文件路径(必填)。
- `-control-listen`:覆盖 `control_listen_addr`。
- `-insecure-allow-remote-control`:允许把控制面绑到非回环地址**且不开 auth**(危险,详见第 8 节)。

**fail-closed 启动检查**:控制面绑到非回环地址(`0.0.0.0`/`::`/非回环 IP/`:port`)**且**既没开 `auth` 也没配 mTLS(`control_tls.client_ca_file`)时,daemon **拒绝启动**。三种"安全暴露"方式:① 回环;② 开 `auth`(任意地址);③ mTLS(`client_ca_file`,任意地址)。确要裸奔暴露才用 `-insecure-allow-remote-control`。

长期运行请用第 4.1 节的 `vmflow service install` 注册为系统服务(开机自启 + 崩溃自动重启)。

### 4.1 作为系统服务运行(开机自启)

```bash
sudo vmflow service install --config /etc/vmflow/config.yaml
```

**安全要求**:`service install` 会先解析 `--binary`/当前可执行文件的符号链接,并拒绝注册相对路径、普通用户可写的二进制,或父目录可被普通用户写入的路径。不要直接从 `/tmp`、`/home`、Downloads 等位置注册 root/SYSTEM 服务;先把 `vmflow` 安装到受保护路径,例如 `/usr/local/bin/vmflow`、`/opt/vmflow/vmflow` 或 `C:\Program Files\vmflow\vmflow.exe`,必要时用 `--binary` 指向该安装路径。

三平台一条命令注册为原生服务(开机自启 + 崩溃自动重启):

| 平台 | 注册方式 | 日志 | 自动重启 |
|---|---|---|---|
| Linux | systemd unit(`/etc/systemd/system/vmflow.service`) | journald:`journalctl -u vmflow` | `Restart=on-failure RestartSec=5` |
| macOS | launchd daemon(`/Library/LaunchDaemons/io.cloudapp.vmflow.plist`) | `/var/log/vmflow/`(可用 `--log-file` 覆盖) | `KeepAlive=true` |
| Windows | Windows Service(services.msc 可见) | **需 `--log-file`**(SCM 无 stdout) | `sc failure ... actions= restart/5000` |

**Linux 说明**:unit 默认以 root 跑 + `CAP_NET_BIND_SERVICE`(可绑 <1024 特权端口)+ `NoNewPrivileges`。加 `--user vmflow` 改用专用账号(不存在则自动 `useradd --system` 创建)。特权端口转发(到 22/80/443)在 root 下开箱即用。

**Windows 说明**:SCM 启动时无 stdout,务必带 `--log-file C:\ProgramData\vmflow\logs\vmflow.log`。daemon 检测到由 SCM 启动时自动走原生服务模式(报状态、响应 Stop/Shutdown),否则前台运行。

其它命令:
```bash
sudo vmflow service uninstall   # 停止 + 注销 + 删 unit/plist(配置和日志保留,需手动删)
vmflow service status           # 查看服务状态
```

`--extra-args` 可追加 daemon 标志,如 `--extra-args "-control-listen 0.0.0.0:19090"`(注意:暴露控制面仍需开 auth 或走反代/CF Tunnel,见第 8 节)。

> deb/rpm 包自带 systemd unit,`apt install vmflow` 会自动 enable;放好 `/etc/vmflow/config.yaml` 后即开机自启。

---

## 5. 用 `vmflow ctl` 管控

```bash
vmflow ctl [-addr http://127.0.0.1:19090] [-token TOKEN] <health|rules|stats|metrics|precheck|reload>
```

| 子命令 | 方法/路径 | 作用 | 权限 |
|---|---|---|---|
| `health` | `GET /healthz` | 探活、运行规则数 | 任意 |
| `rules` | `GET /v1/rules` | 列正在跑的规则 | 任意 |
| `stats` | `GET /v1/stats` | 每规则上/下行字节、连接数 | 任意 |
| `metrics` | `GET /metrics` | Prometheus 文本指标 | 任意 |
| `precheck` | `POST /v1/precheck` | 校验当前配置(不应用) | 任意 |
| `reload` | `POST /v1/reload` | 重读配置并增量应用 | **admin** |

常用 env:`VMFLOW_CONTROL_TOKEN`(等价 `-token`)。例子:
```bash
vmflow ctl -addr https://ctrl.example.com -token $TOK stats
```
也可以直接 curl(同上方法 + `Authorization: Bearer <token>` 头)。

---

## 6. TUI

```bash
vmflow tui [-addr …] [-token …]
```
实时仪表盘:总览(Dashboard)、规则列表(Rules)、单规则详情(Detail),`[tab]` 在视图间切换;按规则名过滤。

---

## 7. 鉴权模型

- **bearer token**:`Authorization: Bearer <token>`,常量时间比较。
- **角色**:`admin`(可读写,含 `reload`)、`viewer`(只读)。
- **`auth.enabled: false`** = 任何调用者视为 `admin`。**仅限回环**安全(fail-closed 会拦非回环);非回环必须开 auth 或 mTLS。
- **失败限速**:同一对端 IP,60 秒内 10 次 auth 失败 → 锁 60 秒,期间返回 `429`。不信任 `X-Forwarded-For`。
- 用 Bearer 头(非 Cookie)→ 天然防 CSRF / DNS rebinding。

---

## 8. 远程访问控制面(三种姿势)

控制面默认明文 HTTP + 回环。需要远程访问时,按场景选一种:

| 方式 | 适合 | 关键 |
|---|---|---|
| **回环 + 反向代理**(Caddy/Nginx + ACME) | 公网域名 | 零 vmflow 改动;反代管 TLS/证书 |
| **内置 TLS / mTLS**(`control_tls`) | 内网直连、不想架反代 | 配 cert/key(+可选 client_ca_file);客户端 `-tls-*` |
| **Cloudflare Tunnel + Access** | 零入站端口、隐藏源站、SSO | daemon 绑回环;客户端 `-H` 带 Access service token |

**fail-closed 放行表**
| 绑定 | 条件 | 结果 |
|---|---|---|
| 回环 | 无 auth | ✅ 放行(本地单用户) |
| 非回环 + 单向 TLS + 无 auth | — | ❌ 拒绝(加密 ≠ 认证) |
| 非回环 + 开 auth | — | ✅ |
| 非回环 + mTLS(`client_ca_file`) | — | ✅(证书=客户端身份) |
| 非回环 + 无 auth + `-insecure-allow-remote-control` | — | ✅(显式承认风险) |

**内置 TLS 客户端 flag**(4 个客户端入口 `vmflow ctl/tui`、`relayctl`、`relaytui` 通用):
- `-tls-ca-file` / `VMFLOW_TLS_CA_FILE`:校验服务端证书的 CA bundle(私有/自签 CA 用)。
- `-tls-client-cert` / `-tls-client-key`(或 `VMFLOW_TLS_CLIENT_CERT`/`VMFLOW_TLS_CLIENT_KEY`):mTLS 客户端证书。
- `-tls-skip-verify` / `VMFLOW_TLS_INSECURE`:跳过校验(仅排障)。

**通用请求头**(发 Cloudflare Access service token 等):
```bash
vmflow ctl -addr https://ctrl.example.com \
  -H "CF-Access-Client-Id: $CF_ID" -H "CF-Access-Client-Secret: $CF_SECRET" \
  -token $TOK health
# 或 env(分号分隔):VMFLOW_HEADERS="CF-Access-Client-Id: ..; CF-Access-Client-Secret: .."
```

证书来源:公网用 Let's Encrypt(反代或 certbot);内网用 openssl 自签 CA;企业用内部 PKI。注意服务端证书的 **SAN 必须包含客户端连接用的主机名/IP**。详见 [behind-cloudflare.md](behind-cloudflare.md)。

> mTLS 在 Cloudflare 免费层不支持 → 走 CF 时机器身份用 Access service token(`-H`),不要用 `client_ca_file`。

---

## 9. 规则生命周期

- **改配置 → 生效**:在服务器上编辑 `config.yaml`,然后 `vmflow ctl reload`(admin)。reload 会先 `precheck`,失败则不应用。
- **`precheck`** 不应用、只校验:模型校验、重复 `rule_id`、监听端口冲突、可绑定性、目标 DNS 解析、低端口权限等。
- **增量 diff**:reload 用 `RuntimeEqual` 只比较影响运行的字段,只动需要动的规则。
  - 改这些 → **重启**该规则:`protocol` / `listen_addr` / `listen_port` / `target_addr` / `target_port` / `enabled` / `speed_limit` / `max_conn` / `idle_timeout`。
  - 改这些 → **只更新不重启**:`name` / `remark` / `revision` / `created_time` / `updated_time`。
- **`ApplySnapshot(ReplaceAll=true)`**:reload 即此语义——快照里没有的旧规则会被停止/移除。

---

## 10. 转发语义

- **协议**:`tcp` / `udp` / `tcp+udp`(同端口同时转发 TCP 和 UDP)。`http`(正向代理)/ `https`(TLS 终结)当前构建禁用。
- **`max_conn`**:每规则并发连接上限,超限新连接直接关闭。`0` = 不限(暴露端口上不建议)。
- **`speed_limit`**:每连接双向令牌桶速率限制(字节/秒)。`0` = 不限。
- **`idle_timeout`**:每连接空闲超时(秒),静默连接到期回收;`0` = 默认 5 分钟。
- **Stop / reload / 关停有界**:停规则时强制关闭已建立连接,TCP 不会因"粘连接"无限期卡住。
- UDP 有 60 秒空闲会话回收;TCP 靠 `idle_timeout`。

---

## 11. 自更新

```bash
vmflow update --check             # 只检查有没有新版本
vmflow update                     # 更新到最新
vmflow update --version vX.Y.Z    # 更新到指定版本
```
- 从 GitHub Release 下载对应 OS/Arch 的 archive,用 **SHA-256 校验** release 自带的 `checksums.txt`,原子替换当前二进制。
- **信任模型**:checksum 是**完整性**校验,不是签名(authenticity)。防得住传输损坏/MITM,防不住 release 被攻陷(那需要 cosign/GPG 签名,目前未集成)。`install.sh` 同理。
- `dev` 构建不能自更新(需 tagged release 构建)。

---

## 12. 指标(`/metrics`)

Prometheus 文本格式,主要指标族:
- `vmflow_build_info`、`vmflow_uptime_seconds`
- `vmflow_rule_running{rule_id,protocol}`、`vmflow_rule_connections{rule_id,protocol}`
- `vmflow_rule_upload_bytes_total` / `vmflow_rule_download_bytes_total{rule_id,protocol}`
- `vmflow_control_requests_total{method,path,status}`、`vmflow_control_request_duration_seconds_sum{...}`
- `vmflow_reload_total{status}`、`vmflow_rule_apply_total{action,status}`

Grafana/Prometheus 直接 scrape `/metrics`(带上 `Authorization: Bearer`),或 `vmflow ctl metrics` 看一眼。NAT 隧道相关指标(`vmflow_tunnel_control_*`)当前构建不产出。

---

## 13. Telegram bot(可选)

配 `bot_token` + `bot_chat` 后随 daemon 启动。命令:`/start` `/status` `/rules` `/detail <id>` `/reload` `/stop <id>` `/start_rule <id>`。
- **授权**:只响应来自配置 `bot_chat` 的消息/回调(私聊时 chat id = 你的 user id;群聊 chat id 为负,回调按钮在群里可能失效——见已知问题)。
- **`/reload` 注意**:当前实现实际执行 `StopAll`(停全部规则),完整重载请配合控制面 `reload`。token 泄露 = bot 完全控制,保管好。

---

## 14. 作为库嵌入

```go
import "github.com/cloudapp3/vmflow"

rt := vmflow.New()
defer rt.Close()
result := rt.Apply(rules)      // []engine.Rule
stats := rt.SnapshotAll()
```
vmflow 只管"进程内转发 + 规则生命周期 + 实时计数";持久化、鉴权、UI、审计、业务规则由宿主负责。详见 [EMBEDDING.md](EMBEDDING.md)。

---

## 15. 安全加固清单

已内置的控制项:
- ✅ 控制 API 默认回环 + **fail-closed**(非回环无 auth/mTLS 拒绝启动)。
- ✅ 可选 **TLS / mTLS**(`control_tls`)。
- ✅ bearer + 角色 + **失败限速**(429)。
- ✅ 错误响应**不泄露**内部路径;`config_path` 只回文件名。
- ✅ TCP **空闲超时** + **Stop 强制关连接**(防粘连接卡死/资源耗尽)。
- ✅ 自更新**随机临时文件名**(防本地符号链接覆盖)+ `install.sh` `--no-same-owner`。
- ✅ 客户端 `-H` 支持 Cloudflare Access service token。
- ⚠️ 自更新**无签名校验**(供应链信任模型,见第 11 节)——已记录,未集成。
- ⚠️ 客户端证书**吊销**(CRL/OCSP)未集成——mTLS 靠证书有效期 + CA 轮换。

运维侧建议:daemon 绑回环;`auth.enabled: true`;远端走反代或 CF Tunnel;token/证书私钥 `0600` 且不入 git;开启 systemd `Restart=on-failure`。

---

## 16. 排错 / 常见坑

| 现象 | 排查 |
|---|---|
| daemon 启动报 "control API is bound to ... without authentication" | fail-closed 触发:绑回环,或开 auth,或配 mTLS,或加 `-insecure-allow-remote-control` |
| `ctl` 报 `x509: certificate signed by unknown authority` | 自签/私有 CA 时用 `-tls-ca-file` 指向你的 CA |
| TLS 连接报主机名不匹配 | 服务端证书 SAN 没包含客户端 `-addr` 里的主机名/IP |
| 换了 control_tls 证书不生效 | `/v1/reload` 不重建 TLS,**需重启 daemon** |
| 端口起不来 / reload 报冲突 | `ctl precheck` 看端口冲突、可绑定性;确认端口没被占 |
| 远程连不上、超时 | 先 `ctl health` 验链路;确认 daemon 只在回环 + 反代/隧道方向正确 |
| `vmflow update` 失败 | `dev` 构建不支持自更新;检查 GitHub Token 限额;网络 |

---

## 17. 相关文档

- [API 参考](API.md)——控制 API 每个端点的请求/响应。
- [功能清单](FEATURES.md)——按子系统罗列能力(含当前禁用项)。
- [嵌入指南](EMBEDDING.md)——作为库使用的 API。
- [Cloudflare 暴露](behind-cloudflare.md)——Tunnel + Access runbook。
- [架构](ARCHITECTURE.md)、[路线图](ROADMAP.md)、[变更日志](CHANGELOG.md)。
