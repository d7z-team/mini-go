# Mini-Go

[简体中文](./README_zh.md)

[![CI][ci-badge]][ci]

Mini-Go is an embeddable scripting engine with Go-like syntax. Go applications compile and run
scripts directly, Rust applications execute the same bytecode, and the TypeScript runtime supports
browsers and Node.js. The `mini-go` CLI runs `.mgo` files without a host application.

## Highlights

- Go-like language with generics, closures, channels, reflection, and embedded resources.
- Isolated instances with cancellation, resource limits, observability, and hot patching.
- Host capabilities and generated Go, Mini-Go, Rust, and TypeScript MRPC bindings.
- Go, Rust, and TypeScript runtimes backed by the same bytecode contract.
- Formatting, LSP, DAP, caching, and a VS Code extension.

The standard library surface implemented by Mini-Go is listed in the
[generated reference](./docs/reference/README.md).

## Quick start

The Go host toolchain requires Go 1.26 or later:

```bash
go install github.com/d7z-team/mini-go/cmd/mini-go@latest
```

Create `hello.mgo`:

```go
package main

func main() {
	println("Hello, Mini-Go!")
}
```

Check and run it:

```bash
mini-go check hello.mgo
mini-go run hello.mgo
```

Mini-Go source files use `.mgo`; tests use `_test.mgo`. See the [Go and CLI guide](./USAGE.md) for
project layout, host capabilities, execution control, and hot patching.

## Embed in Go

```bash
go get github.com/d7z-team/mini-go
```

An embedding application provides sources, creates an `Engine`, compiles a reusable `Program`, and
instantiates isolated runtime state. Start with the [complete Go example](./USAGE.md#完整嵌入示例).

## Other runtimes

| Environment | Package and guide |
| --- | --- |
| Native Rust | [`mini-go` crate][crate-runtime] · [Rust runtime][rust-runtime] |
| Browser and Node.js | `npm install @d7z-team/mini-go` · [TypeScript runtime][typescript-runtime] |

Use the component guide for packaging, deployment, and toolchain details.

## Documentation

The detailed guides below are written in Chinese.

| Task | Guide |
| --- | --- |
| Embed in Go or use the CLI | [Go and CLI guide](./USAGE.md) |
| Define and connect RPC services | [RPC guide](./RPC.md) |
| Use native Rust | [Rust runtime][rust-runtime] |
| Use Browser or Node.js | [TypeScript runtime][typescript-runtime] |
| Configure the editor | [VS Code extension](./vscode-ext/README.md) |
| Understand component boundaries | [Architecture](./ARCHITECTURE.md) |
| Build, test, diagnose, or release | [Development guide](./DEVELOPMENT.md) |

Maintainer workflows and contribution rules are in the [development guide](./DEVELOPMENT.md) and
[AGENTS.md](./AGENTS.md).

## License

Mini-Go uses the [MIT License](./LICENSE). Ported Go standard-library sources retain their original
copyright notices and use the [Go license](./stdlib/LICENSE_GO).

[rust-runtime]: ./playground/runtime-rust/README.md
[typescript-runtime]: ./playground/runtime-typescript/README.md
[crate-runtime]: https://crates.io/crates/mini-go
[ci-badge]: https://github.com/d7z-team/mini-go/actions/workflows/publish.yml/badge.svg
[ci]: https://github.com/d7z-team/mini-go/actions/workflows/publish.yml
