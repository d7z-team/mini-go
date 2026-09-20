# Mini-Go

[简体中文](./README_zh.md)

[![Go tests and coverage][go-ci-badge]][go-ci]

Mini-Go is an embeddable scripting engine with a Go-like syntax. Go applications can compile and
invoke scripts directly, Rust applications can execute the same bytecode, and browsers and Node.js
can use the WebAssembly SDK. The `mini-go` CLI can also run `.mgo` files directly.

**[Quick start](#quick-start) · [Use from Go](#use-from-go) · [Usage guide](./USAGE.md) ·
[Standard library](./docs/reference/README.md) · [RPC](./RPC.md)**

## Features

- **Go-like language**: generics, closures, channels and select, defer/panic/recover, reflection,
  and embedded resources.
- **Embeddable runtime**: reusable programs, isolated instances, bounded parallel execution,
  cancellation, resource limits, and hot patching.
- **Host integration**: FFI host capabilities and generated MRPC clients for local or remote services.
- **Multiple environments**: native Go and Rust runtimes plus a TypeScript SDK for browsers and Node.js.
- **Tooling**: local source composition, compilation caching, formatting, LSP, DAP, and a VS Code extension.

Mini-Go includes a curated standard library for strings, containers, encoding, templates, and other
common scripting tasks. See the [usage guide](./USAGE.md) and
[standard library reference](./docs/reference/README.md) for the supported language and API surface.

## Installation

Mini-Go requires Go 1.26 or later.

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

The program prints `Hello, Mini-Go!`. Pass multiple files with
`mini-go run main.mgo helper.mgo`. Mini-Go source files use `.mgo`, tests use `_test.mgo`, and Go
host code uses `.go`. Directory mode uses `-module` for the logical import prefix and `-source` for
additional local source trees; see the [CLI guide](./USAGE.md#cli).

## Use from Go

Add the module to the host application:

```bash
go get github.com/d7z-team/mini-go
```

The embedding flow is: provide sources → create an Engine → compile a Program → instantiate it →
call an entry point. Programs are reusable, while each instance owns isolated state. See the
[complete embedding example](./USAGE.md#完整嵌入示例) for executable code and the
[usage guide](./USAGE.md) for source composition, host capabilities, execution control, and hot patching.

## Documentation

The detailed guides linked below are written in Chinese.

| Document | Coverage |
| --- | --- |
| [Usage guide](./USAGE.md) | Embedding API, CLI, source composition, execution control, and debugging |
| [RPC guide](./RPC.md) | Interface declarations, Go handlers, script calls, and resource cleanup |
| [Standard library reference](./docs/reference/README.md) | Generated package and API documentation |
| [VS Code extension](./vscode-ext/README.md) | Syntax highlighting and language server configuration |
| [Rust runtime][rust-runtime] · [usage guide][rust-usage] | Native calls, cancellation, async integration, debugging, and hot patching |
| [Browser and Node.js][wasm-sdk] | TypeScript SDK, Worker deployment, WebAssembly, and language tools |
| [Architecture](./ARCHITECTURE.md) | Component boundaries, data flow, state ownership, and lifecycle |
| [Development guide](./DEVELOPMENT.md) | Generation, testing, cross-language verification, and performance diagnostics |
| [Shared test data](./testdata/README.md) | Cross-backend fixtures, expectations, and update procedures |

[rust-runtime]: ./playground/runtime-rust/README.md
[rust-usage]: ./playground/runtime-rust/USAGE.md
[wasm-sdk]: ./playground/runtime-rust/runtime-wasm/README.md
[go-ci-badge]: https://github.com/d7z-team/mini-go/actions/workflows/go-test.yml/badge.svg
[go-ci]: https://github.com/d7z-team/mini-go/actions/workflows/go-test.yml

## Contributing

Read the [architecture](./ARCHITECTURE.md) and [development guide](./DEVELOPMENT.md) before making
changes. Run `make help` at the repository root to list build, generation, and verification targets.
When reporting an issue, include a minimal `.mgo` program, the command you ran, and the expected and
actual results.

Run `make coverage` to execute the Go test suite with atomic coverage. The command writes
`coverage.txt` for tooling and `coverage.html` for local inspection. CI reports the total in the
workflow summary and publishes both files in the `go-coverage` artifact for 14 days.

## License

Mini-Go is licensed under the [MIT License](./LICENSE). Source ported from the Go standard library
retains its original copyright notices and is covered by the [Go license](./stdlib/LICENSE_GO).
