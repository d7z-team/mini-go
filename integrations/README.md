# 集成测试

本目录通过公开 API 验证跨 compiler/runtime、宿主和进程的行为。
包内单元测试与跨后端共享数据的分工见[测试组织](../DEVELOPMENT.md#测试组织)。

## 场景与数据

| 范围 | 数据来源 |
| --- | --- |
| 诊断、求值顺序、类型、缓存与优化一致性 | 本目录的 `testdata/`，按行为组织 |
| RPC 的 VM 调用、服务发布与跨语言通信 | 根 [RPC 一致性数据](../testdata/README.md#rpc-一致性数据) |
| Go/Rust 原生标准库宿主与调试 | 根 [共享测试数据](../testdata/README.md) |

## 运行

```bash
make test TEST_PACKAGES='./integrations'
```

普通集成测试也由 `make test` 自动发现，并复用 Mini-Go 编译缓存。
跨语言通信矩阵需要先构建 peer，使用[开发指南](../DEVELOPMENT.md#rust-验证)中的 `make test-interop`。
