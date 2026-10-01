# Mini-Go

[English](./README.md)

[![CI][ci-badge]][ci]

Mini-Go 是使用 Go-like 语法的嵌入式脚本引擎。Go 应用直接编译并调用脚本，Rust 应用执行同一字节码，
浏览器与 Node.js 通过 WebAssembly SDK 接入；`mini-go` CLI 也可以独立运行 `.mgo` 文件。

**[快速开始](#快速开始) · [Go](./USAGE.md) · [Rust](./playground/runtime-rust/README.md) · [浏览器 / Node.js](./playground/runtime-rust/runtime-wasm/README.md) · [RPC](./RPC.md)**

## 主要功能

- **Go-like 语言**：泛型、闭包、channel/select、defer/panic/recover、反射和嵌入资源。
- **受控执行**：独立 Instance、有界并行、取消、资源限制、观测和热更新。
- **宿主扩展**：通过 FFI 接入宿主能力，生成 Go、Mini-Go、Rust 与 TypeScript MRPC 客户端和 Provider。
- **多环境接入**：Go runtime、Rust runtime，以及面向浏览器和 Node.js 的 TypeScript SDK。
- **工具链**：本地源码装配、编译缓存、格式化、LSP、DAP 和 VS Code 扩展。

精选标准库覆盖字符串、容器、编码、模板等常用脚本能力。支持的 API 见[标准库参考](./docs/reference/README.md)。

## 安装

需要 Go 1.26 或更新版本。

```bash
go install github.com/d7z-team/mini-go/cmd/mini-go@latest
```

请将 Go 的可执行文件安装目录（`GOBIN`，未设置时为 `GOPATH/bin`）加入 `PATH`。
作为 Go 依赖使用时，见下方[在 Go 中使用](#在-go-中使用)。

## 快速开始

创建 `hello.mgo`：

```go
package main

func main() {
	println("Hello, Mini-Go!")
}
```

```bash
mini-go check hello.mgo
mini-go run hello.mgo
```

输出 `Hello, Mini-Go!`。Mini-Go 源码使用 `.mgo`，测试文件使用 `_test.mgo`。
多文件、目录和本地模块的使用方式见[命令行指南](./USAGE.md#cli)。

## 在 Go 中使用

在宿主项目中添加依赖：

```bash
go get github.com/d7z-team/mini-go
```

嵌入流程为：提供源码 → 创建 Engine → 编译 Program → 创建 Instance → 调用入口。
Program 可复用，实例状态相互独立。[使用指南](USAGE.md)包含完整示例，并说明源码装配、宿主能力、
执行控制和热更新。

## 其他运行环境

| 环境 | 安装与指南 |
| --- | --- |
| Rust 原生 | [`mini-go` crate](https://crates.io/crates/mini-go) · [运行时与可选编译工具](./playground/runtime-rust/README.md) |
| 浏览器与 Node.js | `npm install @d7z-team/mini-go@git` · [TypeScript SDK](./playground/runtime-rust/runtime-wasm/README.md) |

Rust 与 npm 包均包含匹配的编译器资源，安装和部署方式见对应组件指南。

## 文档

| 任务 | 指南 |
| --- | --- |
| 嵌入 Go 或使用 CLI | [使用指南](./USAGE.md) |
| 定义和接入 RPC 服务 | [RPC 指南](./RPC.md) |
| 查询标准库 API | [生成参考](./docs/reference/README.md) |
| 配置编辑器 | [VS Code 扩展](./vscode-ext/README.md) |
| 理解组件边界和状态归属 | [架构](./ARCHITECTURE.md) |
| 构建、测试、诊断和发布 | [开发指南](./DEVELOPMENT.md) |

[ci-badge]: https://github.com/d7z-team/mini-go/actions/workflows/publish.yml/badge.svg
[ci]: https://github.com/d7z-team/mini-go/actions/workflows/publish.yml

## 参与开发

开始修改前阅读[架构](./ARCHITECTURE.md)与[开发指南](./DEVELOPMENT.md)。在仓库根运行
`make help` 查看构建与验证入口。提交问题时请附上最小 `.mgo` 示例、执行命令、预期行为和实际结果。

## 许可证

项目使用 [MIT License](./LICENSE)。从 Go 标准库移植的源码保留其版权声明，适用 [Go 许可证](./stdlib/LICENSE_GO)。
