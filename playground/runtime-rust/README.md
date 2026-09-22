# Rust 运行时

Rust 后端执行 Go 编译器生成的 Mini-Go 字节码，支持异步 FFI、调试、资源限制和热更新。
Program 可共享，每个实例持有独立状态。浏览器与 Node.js 接入见
[runtime-wasm](https://github.com/d7z-team/mini-go/blob/main/playground/runtime-rust/runtime-wasm/README.md)。

按任务查阅：[快速开始](#快速开始) · [原生使用指南](USAGE.md) · [功能配置](#功能配置) · [接入导航](#接入导航) ·
[语言工具](#编译器与语言工具)。

## 快速开始

使用 Rust 1.98 或更新版本。从 [crates.io](https://crates.io/crates/mini-go) 选择一个提交快照，
以精确版本添加 runtime：

```toml
[dependencies]
mini-go = "=<snapshot-version>"
```

从源码仓库联调时可改用
`mini-go = { path = "/path/to/go-mini/playground/runtime-rust" }`。

在仓库根目录运行预编译示例：

```bash
cargo run --manifest-path playground/runtime-rust/Cargo.toml --example precompiled -- \
  playground/runtime-rust/examples/blocks/arithmetic.json 10
```

结果为 55。同目录的 `closure.json` 演示闭包，`stateful.json` 演示实例状态。
在自己的项目中加载镜像、调用与关闭实例，见[完整 Rust 示例](USAGE.md#加载调用与限制)。

从仓库根目录生成自己的预编译镜像：

```bash
GOTOOLCHAIN=go1.26.6 go run ./cmd/mini-go-dev runtime-blocks -out /tmp/blocks path/to/block.mgo
```

镜像与 runtime 必须使用匹配的工具链契约。仓库示例由根 `make generate` 更新。

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

## 接入导航

| 任务 | 文档 |
| --- | --- |
| 调用、限制、取消与 Tokio | [原生使用指南](USAGE.md) |
| 标准库与自定义宿主 | [宿主能力](USAGE.md#宿主能力) |
| RPC 生成与服务接入 | [Rust RPC API](https://github.com/d7z-team/mini-go/blob/main/RPC.md#rust-api) |
| 断点、变量和热更新 | [调试](USAGE.md#断点变量与单步) · [热更新](USAGE.md#热更新) |
| 内存与性能观测 | [开发指南](https://github.com/d7z-team/mini-go/blob/main/DEVELOPMENT.md#缓存与性能) |

## 编译器与语言工具

需要从 Rust 编译源码时，启用 `compiler`：

```toml
[dependencies]
mini-go = { version = "=<snapshot-version>", features = ["compiler"] }
```

发布的 crate 已包含编译器镜像；源码 checkout 先在仓库根执行 `make runtime-compiler-image`。
编译会话复用 Go 编译器的语言与源码装配规则，调用方式见[本地源码与编译器工具](USAGE.md#本地源码与编译器工具)。

## 开发与许可证

构建、生成、一致性验证和基准命令统一见
[开发指南](https://github.com/d7z-team/mini-go/blob/main/DEVELOPMENT.md#rust-验证)，数据入口见
[共享测试数据](https://github.com/d7z-team/mini-go/blob/main/testdata/README.md)，内部职责见
[架构](https://github.com/d7z-team/mini-go/blob/main/ARCHITECTURE.md)。

本 crate 使用仓库 MIT 许可证，移植的 Go 算法保留 [Go 许可证](LICENSE-Go)。
