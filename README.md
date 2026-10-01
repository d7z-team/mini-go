# Mini-Go

[简体中文](./README_zh.md)

[![CI][ci-badge]][ci]

Mini-Go is an embeddable scripting engine with Go-like syntax. Go applications compile and invoke
scripts directly, Rust applications execute the same bytecode, and browsers and Node.js use the
WebAssembly SDK. The `mini-go` CLI also runs `.mgo` files without a host application.

**[Quick start](#quick-start) · [Go](./USAGE.md) · [Rust][rust-runtime] · [Browser / Node.js][wasm-sdk] · [RPC](./RPC.md)**

## Features

- **Go-like language**: generics and generic methods, promoted-field literals, closures, channels and select, defer/panic/recover, reflection, and embedded resources.
- **Controlled execution**: isolated instances, bounded parallelism, cancellation, limits, observability, and hot patching.
- **Host integration**: FFI capabilities and generated Go, Mini-Go, Rust, and TypeScript MRPC bindings.
- **Portable runtime**: native Go and Rust backends plus a TypeScript SDK for browsers and Node.js.
- **Tooling**: local source composition, caching, formatting, LSP, DAP, and a VS Code extension.

The curated standard library covers strings, containers, encoding, templates, and common scripting tasks.
See the [standard library reference](./docs/reference/README.md) for supported APIs.

## Installation

The host toolchain requires Go 1.26 or later. Mini-Go's language and standard library have their own
support scope; see [language and standard library](./USAGE.md#语言与标准库).

```bash
go install github.com/d7z-team/mini-go/cmd/mini-go@latest
```

Add Go's binary installation directory (`GOBIN`, or `GOPATH/bin` when `GOBIN` is unset) to `PATH`.
To embed Mini-Go as a Go dependency, continue with [Use from Go](#use-from-go).

## Quick start

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

The program prints `Hello, Mini-Go!`. Mini-Go source files use `.mgo` and tests use `_test.mgo`.
For multiple files, directories, and local modules, see the [CLI guide](./USAGE.md#cli).

## Use from Go

Add the module to the host application:

```bash
go get github.com/d7z-team/mini-go
```

The embedding flow is: provide sources → create an Engine → compile a Program → instantiate it →
call an entry point. Programs are reusable; each instance owns isolated state. The
[usage guide](./USAGE.md) contains a complete example and covers source composition, host
capabilities, execution control, and hot patching.

## Other runtimes

| Environment | Package and guide |
| --- | --- |
| Native Rust | [`mini-go` crate][crate-runtime] · [Runtime and optional compiler tools][rust-runtime] |
| Browser and Node.js | `npm install @d7z-team/mini-go@git` · [TypeScript SDK][wasm-sdk] |

Rust and npm packages include matching compiler resources. Follow the component guides for installation and deployment.

## Documentation

The detailed guides linked below are written in Chinese.

| Task | Guide |
| --- | --- |
| Embed in Go or use the CLI | [Usage guide](./USAGE.md) |
| Define and connect RPC services | [RPC guide](./RPC.md) |
| Look up standard library APIs | [Generated reference](./docs/reference/README.md) |
| Set up the editor | [VS Code extension](./vscode-ext/README.md) |
| Understand component boundaries and ownership | [Architecture](./ARCHITECTURE.md) |
| Build, test, diagnose, or release changes | [Development guide](./DEVELOPMENT.md) |

[rust-runtime]: ./playground/runtime-rust/README.md
[wasm-sdk]: ./playground/runtime-rust/runtime-wasm/README.md
[crate-runtime]: https://crates.io/crates/mini-go
[ci-badge]: https://github.com/d7z-team/mini-go/actions/workflows/publish.yml/badge.svg
[ci]: https://github.com/d7z-team/mini-go/actions/workflows/publish.yml

## Contributing

Read the [architecture](./ARCHITECTURE.md) and [development guide](./DEVELOPMENT.md) before making
changes. Run `make help` at the repository root to list build and verification targets. When
reporting an issue, include a minimal `.mgo` program, the command, and the expected and actual result.

## License

Mini-Go is licensed under the [MIT License](./LICENSE). Source ported from the Go standard library
retains its original copyright notices and is covered by the [Go license](./stdlib/LICENSE_GO).
