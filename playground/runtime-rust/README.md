# Rust 运行时

Rust 后端执行 Go 编译器生成的 Mini-Go 字节码，支持异步 FFI、调试、资源限制和热更新。
Program 可共享，每个实例持有独立状态。浏览器与 Node.js 接入见
[runtime-wasm](https://github.com/d7z-team/mini-go/blob/main/playground/runtime-rust/runtime-wasm/README.md)。

[快速开始](#快速开始) · [功能配置](#功能配置) · [原生使用指南](USAGE.md) · [开发流程](#开发与许可证)

## 快速开始

使用 Rust 1.98 或更新版本。从 [crates.io](https://crates.io/crates/mini-go) 选择一个提交快照，
以精确版本添加 runtime：

```toml
[dependencies]
mini-go = "=<snapshot-version>"
```

源码联调可使用 `mini-go = { path = "/path/to/go-mini/playground/runtime-rust" }`。

在仓库根目录运行预编译示例：

```bash
cargo run --manifest-path playground/runtime-rust/Cargo.toml --example precompiled -- \
  playground/runtime-rust/examples/blocks/arithmetic.json 10
```

结果为 55。同目录的 `closure.json` 演示闭包，`stateful.json` 演示实例状态。
在自己的项目中加载镜像、调用与关闭实例，见[完整 Rust 示例](USAGE.md#加载调用与限制)。

镜像与 runtime 必须使用匹配的工具链契约。生成自己的镜像见
[预编译示例](https://github.com/d7z-team/mini-go/blob/main/DEVELOPMENT.md#生成预编译示例)。

## 功能配置

| Cargo feature | 提供内容 |
| --- | --- |
| 默认 | VM、镜像加载、执行、调试和热更新 |
| `compiler` | CompilerSession、LanguageService 与源码数据类型；原生 bundled 入口内嵌编译器 |
| `dap` | 原生与 WASM 共用的 DAP 调试会话 |
| `language-server` | compiler + dap；原生 LSP 与 Tokio 协议流适配 |
| `rpc` | Provider、客户端、资源、Router、Endpoint 与 FFI Host |
| `rpc-gateway` | WebSocket/TLS、Unix socket、认证与服务发布传输 |
| `stdlib-host` | 原生 console、环境与内存文件系统 |

调用、取消、调试、热更新与编译会话见[原生使用指南](USAGE.md)；多语言服务接入见
[Rust RPC API](https://github.com/d7z-team/mini-go/blob/main/RPC.md#rust-api)。

## 开发与许可证

构建、生成、一致性验证和基准命令统一见
[开发指南](https://github.com/d7z-team/mini-go/blob/main/DEVELOPMENT.md#rust-验证)，组件职责见
[架构](https://github.com/d7z-team/mini-go/blob/main/ARCHITECTURE.md)。

本 crate 使用仓库 MIT 许可证，移植的 Go 算法保留 [Go 许可证](LICENSE-Go)。
