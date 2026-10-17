# Mini-Go Rust Runtime

这是 Mini-Go 的 Rust 原生运行时 crate。它加载 Go compiler 生成的 bytecode，提供实例执行、宿主 FFI、
资源限制、调试和热更新。一个 `Program` 可以由多个实例共享；每个实例拥有独立的执行状态。
浏览器和 Node.js 使用 [TypeScript runtime](../runtime-typescript/README.md)。

## 安装与快速开始

发布依赖使用固定版本：

```toml
[dependencies]
mini-go = "=<snapshot-version>"
```

仓库内可以直接运行预编译示例：

```bash
cargo run --manifest-path playground/runtime-rust/Cargo.toml --example precompiled -- \
  playground/runtime-rust/examples/blocks/arithmetic.json 10
```

示例返回 `55`。加载镜像、启动执行和关闭实例见[原生使用指南](USAGE.md)。镜像必须由匹配的工具链生成；
镜像生成和验证入口见根 [开发指南](../../DEVELOPMENT.md#生成预编译示例)。

## Features

| Feature | 内容 |
| --- | --- |
| 默认 | VM、镜像加载、执行、调试和热更新 |
| `compiler` | `CompilerSession`、`LanguageService` 和匹配的 compiler 镜像 |
| `dap` | 原生 DAP 调试会话 |
| `language-server` | compiler、DAP 和 Tokio 协议流适配；同时启用 `compiler` 与 `dap` |
| `rpc` | provider、client、Router、Endpoint 和 FFI host |
| `rpc-gateway` | WebSocket/TLS、Unix socket、认证和服务发布 |
| `stdlib-host` | console、environment 和内存文件系统；同时启用 `rpc` |

RPC 的跨语言用法见根 [RPC 指南](../../RPC.md#rust)。

## 文档导航

- [Rust 原生使用指南](USAGE.md)：公开执行 API、取消与关闭、FFI、并行、调试和热更新。
- [根开发指南](../../DEVELOPMENT.md#rust-验证)：依赖、生成、测试和发布验证。
- [根架构说明](../../ARCHITECTURE.md)：组件边界与生命周期。

本 crate 使用仓库 MIT 许可证；移植的 Go 算法保留 [Go License](LICENSE-Go)。
