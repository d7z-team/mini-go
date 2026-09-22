# Mini-Go

[English](./README.md)

[![Go 测试与覆盖率][go-ci-badge]][go-ci]

Mini-Go 是使用 Go-like 语法的嵌入式脚本引擎。Go 应用直接编译并调用脚本，Rust 应用执行同一字节码，
浏览器与 Node.js 通过 WebAssembly SDK 接入；`mini-go` CLI 也可以独立运行 `.mgo` 文件。

**[快速开始](#快速开始) · [嵌入 Go](#在-go-中使用) · [使用指南](./USAGE.md) · [标准库](./docs/reference/README.md) · [RPC](./RPC.md)**

## 主要功能

- **Go-like 语言**：泛型、闭包、channel/select、defer/panic/recover、反射和嵌入资源。
- **受控执行**：独立 Instance、有界并行、取消、资源限制、观测和热更新。
- **宿主扩展**：通过 FFI 接入宿主能力，生成 Go、Mini-Go、Rust 与 TypeScript MRPC 客户端和 Provider。
- **多环境接入**：Go runtime、Rust runtime，以及面向浏览器和 Node.js 的 TypeScript SDK。
- **工具链**：本地源码装配、编译缓存、格式化、LSP、DAP 和 VS Code 扩展。

随引擎提供字符串、容器、编码、模板等精选标准库。Mini-Go 面向脚本场景，语言与 API 的支持范围见
[使用指南](./USAGE.md)和[标准库参考](./docs/reference/README.md)。

## 安装

需要 Go 1.26 或更新版本。

```bash
go install github.com/d7z-team/mini-go/cmd/mini-go@latest
```

请将 Go 的可执行文件安装目录（`GOBIN`，未设置时为 `GOPATH/bin`）加入 `PATH`。
作为 Go 依赖使用时，见下方[嵌入示例](#在-go-中使用)。

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

输出 `Hello, Mini-Go!`。多文件可写为 `mini-go run main.mgo helper.mgo`。
Mini-Go 源码统一使用 `.mgo`，测试文件使用 `_test.mgo`；Go 宿主代码使用 `.go`。
目录模式用 `-module` 声明导入前缀，额外本地源码通过 `-source` 提供，详见[命令行使用](./USAGE.md#cli)。

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

Rust 与 npm 分发均包含匹配的编译器资源。快照安装见组件指南，版本规则与发布流程见
[开发指南](./DEVELOPMENT.md#rust-与-npm-发布)。

## 文档

| 文档 | 内容 |
| --- | --- |
| [使用指南](./USAGE.md) | 嵌入 API、CLI、源码装配、执行控制与调试 |
| [RPC 使用指南](./RPC.md) | 声明接口、生成多语言 binding、跨语言调用和资源关闭 |
| [标准库参考](./docs/reference/README.md) | 从源码生成的包与 API 文档 |
| [VS Code 扩展](./vscode-ext/README.md) | 语法高亮与语言服务配置 |
| [Rust 运行时](./playground/runtime-rust/README.md) · [使用指南](./playground/runtime-rust/USAGE.md) | 原生调用、取消、异步接入、调试与热更新 |
| [浏览器与 Node.js](./playground/runtime-rust/runtime-wasm/README.md) | TypeScript SDK、Worker、WASM 与语言工具 |
| [架构](./ARCHITECTURE.md) | 组件边界、数据流、状态所有权与生命周期 |
| [开发指南](./DEVELOPMENT.md) | 生成、测试、跨语言验证与性能诊断 |
| [共享测试数据](./testdata/README.md) | 跨后端语料、预期与更新入口 |

[go-ci-badge]: https://github.com/d7z-team/mini-go/actions/workflows/go-test.yml/badge.svg
[go-ci]: https://github.com/d7z-team/mini-go/actions/workflows/go-test.yml

## 参与开发

开始修改前阅读[架构](./ARCHITECTURE.md)与[开发指南](./DEVELOPMENT.md)。在仓库根运行
`make help` 查看构建与验证入口。提交问题时请附上最小 `.mgo` 示例、执行命令、预期行为和实际结果。

## 许可证

项目使用 [MIT License](./LICENSE)。从 Go 标准库移植的源码保留其版权声明，适用 [Go 许可证](./stdlib/LICENSE_GO)。
