# vmflow architecture

## 定位

`vmflow` 是一个独立的纯 L4 端口转发项目，分成三层：

1. `engine/`
   - 纯转发引擎
   - 负责规则校验、启动、重启、停止、流量统计
2. `controlapi/`
   - 本地管理接口
   - 负责 health、stats、reload
3. `cmd/`
   - `relayd`：守护进程
   - `relayctl`：本地控制 CLI

## 核心原则

- 引擎不依赖数据库
- 引擎不依赖特定控制面
- 配置文件与运行态解耦
- 优先保证 Linux，可跨平台编译

## 数据流

```text
config file -> relayd -> config.Load -> engine.Manager.ApplySnapshot
                                   -> controlapi (/healthz /v1/stats /v1/reload)
relayctl -> controlapi -> runtime -> manager
```

## 后续扩展方向

- Prometheus metrics
- graceful drain / force stop
- per-rule 全局带宽桶
- 热更新监听地址变更策略
- 持久化事件日志
