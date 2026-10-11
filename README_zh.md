# Mini-Go

[English](./README.md)

[![CI][ci-badge]][ci]

Mini-Go 是使用 Go-like 语法的嵌入式脚本引擎。Go 应用直接编译并运行脚本，Rust 应用执行同一字节码，
TypeScript 运行时支持浏览器和 Node.js；`mini-go` CLI 也可以脱离宿主应用运行 `.mgo` 文件。

## 主要能力

- 支持泛型、闭包、channel、反射和嵌入资源的 Go-like 语言。
- 支持取消、资源限制、观测和热更新的隔离实例。
- 宿主能力，以及 Go、Mini-Go、Rust 和 TypeScript MRPC 生成代码。
- 共用字节码契约的 Go、Rust 和 TypeScript 运行时。
- 格式化、LSP、DAP、缓存和 VS Code 扩展。

Mini-Go 实现的标准库表面以[生成参考](./docs/reference/README.md)为准。

## 安装与运行

Go 宿主工具链需要 Go 1.26 或更新版本：

```bash
go install github.com/d7z-team/mini-go/cmd/mini-go@latest
```

创建 `hello.mgo`：

```go
package main

func main() {
	println("Hello, Mini-Go!")
}
```

检查并运行：

```bash
mini-go check hello.mgo
mini-go run hello.mgo
```

Mini-Go 源码使用 `.mgo`，测试文件使用 `_test.mgo`。多文件项目、本地模块、宿主能力、执行控制和
热更新见 [Go 与 CLI 使用指南](./USAGE.md)。

## 嵌入 Go

```bash
go get github.com/d7z-team/mini-go
```

宿主提供源码并创建 `Engine`，编译出可复用的 `Program`，再为每份隔离状态创建实例。
从[完整 Go 示例](./USAGE.md#完整嵌入示例)开始接入。

## 其他运行环境

| 环境 | 安装与指南 |
| --- | --- |
| Rust 原生 | [`mini-go` crate](https://crates.io/crates/mini-go) · [Rust 运行时](./playground/runtime-rust/README.md) |
| 浏览器与 Node.js | `npm install @d7z-team/mini-go` · [TypeScript 运行时](./playground/runtime-typescript/README.md) |

编译器资源与运行时产物必须来自同一工具链版本。打包和部署方式见对应组件指南。

## 文档

| 任务 | 指南 |
| --- | --- |
| 嵌入 Go 或使用 CLI | [Go 与 CLI 使用指南](./USAGE.md) |
| 定义和接入 RPC 服务 | [RPC 指南](./RPC.md) |
| 使用 Rust 原生运行时 | [Rust 运行时](./playground/runtime-rust/README.md) |
| 使用浏览器或 Node.js | [TypeScript 运行时](./playground/runtime-typescript/README.md) |
| 配置编辑器 | [VS Code 扩展](./vscode-ext/README.md) |
| 理解组件边界 | [架构](./ARCHITECTURE.md) |
| 构建、测试、诊断和发布 | [开发指南](./DEVELOPMENT.md) |

在仓库根运行 `make help` 查看维护命令，贡献约定见 [AGENTS.md](./AGENTS.md)。

## 许可证

项目使用 [MIT License](./LICENSE)。从 Go 标准库移植的源码保留原版权声明，适用
[Go 许可证](./stdlib/LICENSE_GO)。

[ci-badge]: https://github.com/d7z-team/mini-go/actions/workflows/publish.yml/badge.svg
[ci]: https://github.com/d7z-team/mini-go/actions/workflows/publish.yml
