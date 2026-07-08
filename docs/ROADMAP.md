# roadmap

## v0.1

- [x] TCP 转发
- [x] UDP 转发
- [x] `ApplySnapshot`
- [x] daemon + CLI
- [x] 本地 control API
- [x] YAML 配置

## v0.2

- [x] Prometheus metrics
- [x] 更好的结构化日志
- [x] Control API 鉴权
- [ ] graceful drain
- [x] rule precheck
- [ ] Windows / macOS 手工验证

## v0.3

- [ ] per-rule 共享带宽桶
- [ ] 事件订阅接口
- [ ] 配置热更新策略增强
- [x] systemd / launchd / Windows Service 开机自启(`vmflow service install`,deb/rpm 带 unit)
- [ ] Docker 官方示例
