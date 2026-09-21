# 运行时一致性数据

Go 的 `tooling/runtimecheck`、Rust 与 WASM 测试消费共享语料，验证执行结果与生命周期。

| 输入 | 用途 |
| --- | --- |
| `source/`、`execution_expected.json` | 源程序与手写预期 |
| `execution.json.gz` | 各优化级别的镜像、调试符号与 Go 执行观察 |
| `memory_*.json`、`state.json` | 最小字节码及调度/内存状态观察 |
| `step_limits.json` | 手写累计步数配置：默认、无限、有限与非法边界 |
| `wire.json` | canonical 编码与 hash |
| `stdlib.json.gz` | 两种宿主执行的完整标准库测试镜像 |
| `source/parallel_*`、`source/select_transaction` | 跨后端并行、共享状态和 select 原子提交场景 |

Go 与 Rust 使用不同并行度独立验证共享场景；`parallel_cpu` 也作为固定工作量的性能基线。

修改源码与手写预期后运行根 `make generate`，更新派生物和 manifest。
预编译 `.json.gz` 镜像由本地生成；首次独立消费语料前执行 `make runtime-artifacts`。
manifest 校验数据来源与完整性。源码场景按行为组织，各后端按覆盖范围验证不同优化级别。

运行与宿主一致性验证命令见[开发指南](../../DEVELOPMENT.md#rust-验证)。
