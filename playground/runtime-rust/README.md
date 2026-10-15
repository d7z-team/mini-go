# Mini-Go Rust Runtime

Rust runtime 执行 Go compiler 生成的 Mini-Go bytecode，提供异步 FFI、资源限制、调试和热更新。
`Program` 可以在多个实例间共享，每个实例拥有独立的执行状态。浏览器和 Node.js 使用
[TypeScript runtime](../runtime-typescript/README.md)。

## 安装与快速开始

发布版本使用 crate 的固定版本：

```toml
[dependencies]
mini-go = "=<snapshot-version>"
```

源码联调可以使用路径依赖：

```toml
[dependencies]
mini-go = { path = "/path/to/go-mini/playground/runtime-rust" }
```

仓库内的预编译示例：

```bash
cargo run --manifest-path playground/runtime-rust/Cargo.toml --example precompiled -- \
  playground/runtime-rust/examples/blocks/arithmetic.json 10
```

该示例返回 55。加载镜像、启动调用和关闭实例见[原生使用指南](USAGE.md)。镜像必须由匹配的工具链
生成；生成预编译镜像见[开发指南](../../DEVELOPMENT.md#生成预编译示例)。

## Cargo features

| Feature | 提供内容 |
| --- | --- |
| 默认 | VM、镜像加载、执行、调试和热更新 |
| `compiler` | `CompilerSession`、`LanguageService` 和匹配的 compiler 镜像 |
| `dap` | 原生 DAP 调试会话 |
| `language-server` | compiler、DAP 和 Tokio 协议流适配 |
| `rpc` | provider、客户端、Router、Endpoint 和 FFI host |
| `rpc-gateway` | WebSocket/TLS、Unix socket、认证和服务发布 |
| `stdlib-host` | console、环境和内存文件系统 |

启用 `language-server` 会同时启用 `compiler` 和 `dap`；`stdlib-host` 会同时启用 `rpc`。
RPC 的跨语言使用方式见[RPC 指南](../../RPC.md#rust-api)。

## 相关文档

- [Rust 原生使用指南](USAGE.md)：运行实例、取消关闭、FFI、调试、热更新和 compiler session。
- [开发指南](../../DEVELOPMENT.md#rust-验证)：构建、生成和验证。
- [架构说明](../../ARCHITECTURE.md)：组件边界和生命周期。

本 crate 使用仓库 MIT 许可证；移植的 Go 算法保留 [Go License](LICENSE-Go)。
