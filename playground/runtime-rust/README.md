# Rust 运行时

Rust 后端执行 Go compiler 生成的 Mini-Go bytecode，支持异步 FFI、调试、资源限制和热更新。
Program 可共享，每个实例持有独立状态。Browser 与 Node.js 使用
[TypeScript 运行时](https://github.com/d7z-team/mini-go/blob/main/playground/runtime-typescript/README.md)。

## 安装与快速开始

使用 Rust 1.98 或更新版本，并固定 crate 快照版本：

```toml
[dependencies]
mini-go = "=<snapshot-version>"
```

源码联调可改用 `mini-go = { path = "/path/to/go-mini/playground/runtime-rust" }`。

在仓库根运行预编译示例：

```bash
cargo run --manifest-path playground/runtime-rust/Cargo.toml --example precompiled -- \
  playground/runtime-rust/examples/blocks/arithmetic.json 10
```

该示例返回 55。加载自己的镜像、调用、取消和关闭方式见[原生使用指南](USAGE.md)。镜像与 runtime
必须使用匹配的工具链契约；生成镜像见
[预编译示例](https://github.com/d7z-team/mini-go/blob/main/DEVELOPMENT.md#生成预编译示例)。

## Cargo features

| Feature | 内容 |
| --- | --- |
| 默认 | VM、镜像加载、执行、调试和热更新 |
| `compiler` | CompilerSession、LanguageService 和匹配的 compiler 镜像 |
| `dap` | 原生 DAP 调试会话 |
| `language-server` | compiler + dap + Tokio 协议流适配 |
| `rpc` | provider、客户端、Router、Endpoint 和 FFI Host |
| `rpc-gateway` | WebSocket/TLS、Unix socket、认证和服务发布 |
| `stdlib-host` | console、环境和内存文件系统 |

RPC 用法见
[Rust RPC API](https://github.com/d7z-team/mini-go/blob/main/RPC.md#rust-api)。

## 开发与许可证

构建、生成、验证和 benchmark 见
[开发指南](https://github.com/d7z-team/mini-go/blob/main/DEVELOPMENT.md#rust-验证)，组件边界见
[架构](https://github.com/d7z-team/mini-go/blob/main/ARCHITECTURE.md)。

本 crate 使用仓库 MIT 许可证；移植的 Go 算法保留 [Go 许可证](LICENSE-Go)。
