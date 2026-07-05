# Contributing

欢迎提交 issue 和 PR。

## 本地开发

```bash
make fmt
make test
make build
```

## 贡献原则

- 保持 `engine/` 无业务依赖
- 新能力优先通过小步 PR 引入
- 配置变更要兼容 `version` 字段
- 新增功能要补对应测试

## PR 建议

- 说明变更动机
- 列出影响的文件/模块
- 给出验证方式
