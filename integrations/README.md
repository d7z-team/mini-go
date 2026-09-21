# 集成测试

本目录通过公开 API 验证跨 compiler/runtime、宿主和进程的行为。
单个包独立拥有的测试留在所属包，根包只保留 facade API 测试。

## 场景与数据

| 范围 | 数据来源 |
| --- | --- |
| 诊断、求值顺序、类型、缓存与优化一致性 | 本目录的 `testdata/`，按行为组织 |
| RPC 的 VM 调用、服务发布与跨语言通信 | 根 [testdata/rpc](../testdata/rpc/README.md) |
| Go/Rust 原生标准库宿主与调试 | 根 [共享测试数据](../testdata/README.md) |

测试负责回收实例、连接与子进程。

## 运行

```bash
make test TEST_PACKAGES='./integrations'
make test-rpc-conformance
```

普通集成测试由 `make test` 自动发现，并复用 Mini-Go 编译缓存。
跨语言进程测试由第二条命令先构建 peer，再执行通信矩阵。
