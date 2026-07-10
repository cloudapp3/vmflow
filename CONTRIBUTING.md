# Contributing

Contributions via issues and pull requests are welcome. 欢迎提交 issue 和 PR。

## Local development / 本地开发

```bash
make fmt
make test
make build
```

## Principles / 贡献原则

- Keep `engine/` free of business dependencies — 保持 `engine/` 无业务依赖
- Introduce new capabilities through small, incremental PRs — 新能力优先通过小步 PR 引入
- Config changes must stay compatible with the `version` field — 配置变更要兼容 `version` 字段
- Add tests for any new feature — 新增功能要补对应测试

## PR checklist / PR 建议

- Explain the motivation for the change — 说明变更动机
- List the affected files/modules — 列出影响的文件/模块
- Describe how you verified it — 给出验证方式

Before opening a PR, run the full local checks / 提交前请运行完整检查:

```bash
make fmt && make test && make vet && make smoke && make build
```
