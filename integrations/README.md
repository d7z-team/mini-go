# 集成测试

本目录通过公开 API 验证跨 compiler/runtime、宿主和进程的行为。单个包的测试留在所属包，
多个后端共同验证的输入放在根 [testdata](../testdata/README.md)。

## 场景与数据

| 范围 | 数据来源 |
| --- | --- |
| compiler/runtime 语义、缓存与优化一致性 | 本目录的 `testdata/`，按行为组织 |
| RPC 的 VM 调用、服务发布与跨语言通信 | 根 [RPC 数据](../testdata/README.md#rpc-一致性数据) |
| 标准库宿主与调试 | 根 [标准库宿主数据](../testdata/stdlib-host/) 与 [调试数据](../testdata/debug/) |

使用 `make test TEST_PACKAGES='./integrations'` 运行本目录测试，并复用 Mini-Go 编译缓存。
包筛选和跨进程验证见[日常工作流](../DEVELOPMENT.md#日常工作流)；需要 peer 的跨语言通信矩阵使用
[Rust 验证](../DEVELOPMENT.md#rust-验证)中的 `make test-interop`。
