# migration from vmpulse

`vmflow` 源自 `vmpulse` 中的纯 L4 转发模块抽离，首批迁移内容主要包括：

- 规则模型
- Manager 生命周期管理
- TCP 转发
- UDP 转发
- 内存统计 Collector

明确未迁入的部分：

- `netflow.sync` 任务协议
- vmpulse 数据库与历史流量聚合
- Web 控制面
- `public relay` 管理逻辑
- 单端口 TCP gateway

因此，`vmflow` 现在是一个更纯粹的转发产品底座，而不是 `vmpulse` 的缩小版。
