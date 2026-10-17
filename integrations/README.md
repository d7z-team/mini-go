# 集成测试

本目录验证跨 compiler/runtime、宿主和进程的行为。单个包的行为测试留在所属包；多个组件共同消费的
源码、镜像和预期由根 [testdata](../testdata/README.md) 维护。

覆盖编译产物加载、源码语义、缓存和优化、宿主能力、调试，以及 RPC 服务发布、热更新和跨进程通信。
数据目录与事实源分工见[共享测试数据](../testdata/README.md#目录-owner)。

## 运行

运行本目录的 Go 集成测试：

```bash
make test TEST_PACKAGES='./integrations'
```

涉及 Rust peer 或 Go provider 的跨进程矩阵使用 `make test-interop`。测试范围、缓存和生成入口统一见
[开发指南](../DEVELOPMENT.md#日常工作流)；共享数据修改后的生成与验证见
[生成与派生物](../DEVELOPMENT.md#生成与派生物)。
