# 开发指南

本文集中说明仓库维护、生成、测试和诊断。公共 API 见[使用指南](USAGE.md)与
[RPC 指南](RPC.md)，组件边界和生命周期见[架构](ARCHITECTURE.md)。

[日常工作流](#日常工作流) · [生成](#生成与派生物) · [测试](#测试组织) ·
[Rust](#rust-验证) · [WASM](#wasm-与-typescript) · [发布](#rust-与-npm-发布) ·
[性能](#缓存与性能) · [编辑器](#编辑器扩展)

## 开发环境

Go 开发和标准库差分基准使用 Go 1.26.6，Makefile 默认设置 `GOTOOLCHAIN=go1.26.6`。
Rust crate 的最低版本由各 `Cargo.toml` 的 `rust-version` 声明，Node.js 版本由 npm package
的 `engines` 声明；依赖版本由 go.mod、Cargo.lock 和 npm lockfile 固定。

完整生成需要 Go、make、rustfmt。Rust/WASM 开发还需要 Cargo、Node.js、
`wasm32-unknown-unknown` target 和与 Cargo.lock 匹配的 wasm-bindgen-cli。

## 日常工作流

在仓库根运行 `make help` 查看可用目标。先运行行为所属包的测试，再按影响扩大范围：

```bash
make test TEST_PACKAGES='./compiler/semantic' TEST_FLAGS='-run TestName'
make test TEST_PACKAGES='./rpc/... ./integrations'
make coverage
make race RACE_PACKAGES='./rpc/...'
make lint test build
```

`TEST_FLAGS` 替换默认 Go 测试参数；`-count=1` 只跳过 Go 测试结果缓存，Mini-Go 编译缓存仍可复用。
`make coverage` 使用相同的包与参数设置，生成 `coverage.txt` 和 `coverage.html`，CI 同时报告总覆盖率。
同一次 make 调用按依赖顺序执行生成、构建和消费步骤。Go 与 Cargo 内部仍可并行；受限环境可设置
`GOFLAGS=-p=1`、`GOMAXPROCS`、`CARGO_BUILD_JOBS` 和 `RUST_TEST_THREADS`。

| 改动范围 | 最低验证 |
| --- | --- |
| Go 实现或共享行为 | 目标测试；`make lint test build` |
| 并发、取消或资源生命周期 | 相关行为测试和 `make race` 的对应包 |
| compiler、stdlib、schema 或 generator | `make generate` 后运行受影响测试 |
| stdlib API 或源码注释 | `make doc` 和相关测试 |
| Rust 原生实现 | 目标 Cargo 测试、`make runtime-rust-lint`；共享 VM 行为再跑 `make runtime-rust-test` |
| TypeScript / WASM | `make runtime-wasm-test`，以及目标配置的 Rust Clippy |
| RPC 跨语言契约 | 两侧测试和 `make test-rpc-conformance` |
| 手写文档 | 本地链接、命令和示例；不运行文档生成 |

提交前执行 `git diff --check`，确认生成输入和受影响的派生物保持一致，并记录实际运行的验证。

## 生成与派生物

根 [generate.go](generate.go) 是统一生成入口：

| 事实源 | 派生物 |
| --- | --- |
| `.mrpc` 声明 | Go、Mini-Go、Rust、TypeScript binding |
| compiler、bytecode 与标准库源码 | bootstrap bundle 和工具链 identity |
| bytecode 模型与工具 DTO | [spec](spec/README.md) 契约和 Rust 描述 |
| 共享源码与行为观察 | 预编译镜像、差分数据和 manifest |
| compiler 词法与类型事实 | VS Code grammar |

```bash
make generate
make doc
```

生成必须可重复。修改 schema、envelope 或缓存格式时，同时更新所属 version/domain；修改移植源码时
保留许可证和版权头。`docs/reference/` 从标准库源码注释生成，只通过 `make doc` 更新。

Rust 与 npm tooling 使用预编译 compiler 镜像。以下目标只补齐本地缺失的镜像，便于直接运行消费方测试：

```bash
make runtime-artifacts       # runtime execution、stdlib 和 compiler 镜像
make runtime-compiler-image  # 仅 compiler 镜像
```

修改生成输入后仍应执行 `make generate`，不能把“文件已存在”当作内容已更新。生成或构建完成后，再启动
依赖对应产物的测试。

手写文档职责见 [AGENTS.md](AGENTS.md#文档职责)。调查、设计、性能原始数据和实施记录保存在 `/tmp`。

## 测试组织

测试放在行为所属的 Go package 或 Rust 模块。标准库行为使用同目录的 `*_test.mgo`；根目录只验证 facade，
没有单一 owner 的 compiler/runtime 集成场景放在 [integrations](integrations/README.md)。多个后端共享的输入
和预期放在 [testdata](testdata/README.md)，包专有数据保留在所属包。

runtime 状态机优先使用最小 bytecode Program 和精确 `PollSteps`；只有跨层语义才编译 Mini-Go 源码。
测试应验证可观察结果、diagnostic、wire shape、缓存不变量和资源终态，并负责回收实例、连接、资源、
goroutine 和子进程。复杂构造集中在所属 domain 的测试辅助文件；数量断言只用于明确的 ABI、数据 shape、
语言求值或资源计费契约。

重型 compiler、stdlib 和集成测试复用默认磁盘缓存或 `MINIGO_CACHE`。缓存契约、故障注入和阶段单元测试
使用隔离 backend。Go 测试只在验证 CLI、生成代码或跨进程行为时启动子进程；跨语言命令由 Makefile 编排。

## Rust 验证

安装与 feature 见 [Rust README](playground/runtime-rust/README.md)，原生 API 见
[Rust 使用指南](playground/runtime-rust/USAGE.md)。

| 命令 | 范围 |
| --- | --- |
| `make runtime-rust-lint` | rustfmt、workspace 全 feature/target 的 Clippy |
| `make runtime-rust-test` | 默认 VM 测试和 release 编译器/LSP/DAP 测试 |
| `make runtime-rust-rpc-test` | RPC、生成 binding 和 Gateway |
| `make runtime-rust-host-test` | 原生 Host 与清理生命周期 |
| `make runtime-rust-host-conformance` | Rust provider 执行标准库镜像 |
| `make runtime-rust-conformance` | Go provider 经 broker 执行同一镜像 |
| `make test-rpc-conformance` | Go/Rust Endpoint 与 Gateway 互操作 |
| `make runtime-compiler-test` | Rust、Node 和 Chromium 的 compiler tooling |
| `make runtime-rust-bench` | 固定工作量的执行、分配和 GC 基准 |

调度、GC、热更新或帧复用变更应同时覆盖步骤计费、不同并行度、单 worker 多实例、共享状态、等待、
模块初始化、取消、关闭以及 GC/Patch/DAP 停稳。共享工作量见
[runtime testdata](testdata/runtime/README.md)。编译会话测试使用 release 模式并在请求期限内完成。

`runtime-rust-lint` 分别检查 `compiler`、`dap`、`language-server` feature；默认 VM 测试不加载编译器。
`host-conformance` feature 用于经进程 broker 接入 Go provider 的测试，应用接入使用 `stdlib-host`。
编译器资源统一生成到 `playground/runtime-rust/assets/compiler.json.gz`；crate 包含该文件，
原生 `compiler` 按需内嵌，WASM 使用 SDK 的 `dist/tools/compiler.json.gz`，二进制不重复内嵌。

## WASM 与 TypeScript

安装与 Cargo.lock 一致的 wasm-bindgen-cli，并准备 Rust target 和浏览器：

```bash
rustup target add wasm32-unknown-unknown
cargo install wasm-bindgen-cli --version 0.2.128 --locked
npm exec --prefix playground/runtime-rust/runtime-wasm -- playwright install chromium firefox
make runtime-wasm-test
make runtime-wasm-pack
```

`make runtime-wasm-test` 构建 TypeScript、Worker、WASM 和 compiler 分发，严格编译共享 schema 生成的
TypeScript binding，然后验证 codec、owner 生命周期、浏览器、Node、双向 RPC 与安装包。浏览器 runtime
默认测试 Chromium 和 Firefox；`MINIGO_BROWSERS` 可选择引擎，tools 与打包消费测试使用 Chromium。

在 `playground/runtime-rust/runtime-wasm` 中可单独运行 `npm ci`、`npm run build` 和 `npm run lint`；
`npm run build -- --core` 构建不含 RPC 的版本。`WASM_BINDGEN` 可指定绑定工具，
`MINIGO_BUILD_STD=1` 使用 rust-src 和 Cargo build-std。SDK 的接入与部署见
[runtime-wasm README](playground/runtime-rust/runtime-wasm/README.md)。

## Rust 与 npm 发布

`mini-go` crate 与 `@d7z-team/mini-go` npm 包使用同一提交快照版本：

```text
0.0.<git commit count>-git.g<七位 commit ID>
```

源码 manifest 使用 `0.0.0-dev`；发布脚本从完整 Git 历史派生版本，并只在隔离 staging 中改写 manifest。
正式发布要求完整历史和干净工作区。

```bash
make release-script-test
make release-package
make release-verify
```

`release-package` 在 `build/release/` 生成一个 `mini-go` crate 与 npm tarball，不写 registry；`release-verify`
解包产物、核对 compiler 镜像，并以独立 Rust、Node 和 Chromium consumer 验证。调试未提交内容时可设置
`RELEASE_FLAGS=--allow-dirty`，生成的 `.dirty` 版本不能发布。

在 GitHub Actions 手动触发 [Publish Rust and npm packages](.github/workflows/publish.yml)。
它要求最新 main 的同一提交已通过 Go、Rust push CI，随后验证 WASM/SDK 和分发包，再依次发布 crate 与 npm。
发布使用 Trusted Publisher；首次建立包可选择 `bootstrap`，并配置 `CARGO_REGISTRY_TOKEN` 与 `NPM_TOKEN`。
已存在的同版本产物须通过完整性比较。

## RPC 实现维护

公共用法与资源契约见 [RPC 指南](RPC.md)，共享输入见
[RPC testdata](testdata/rpc/README.md)。实现改动按 owner 核对：

| 变更 | 核对重点 |
| --- | --- |
| MRPC schema / generator | 类型、命名、契约身份、四语言 codec、原子输出 |
| FFI / Result | 接收或丢弃、迟到回复、额度和新资源回收 |
| Endpoint | wire、操作状态、租约、控制消息、分片和断线终态 |
| Router / publication | 一致路由快照、原子替换、旧 lease 和关闭责任 |
| TypeScript RPC SDK | Worker owner、两阶段结果、provider/resource 清理与 Browser/Node 一致性 |

协议测试应覆盖消息顺序、背压和最终清理：请求完整发送后才等待接纳，取消不能越过已接受的工作；
续租只确认对应批次，失效授权不能恢复；资源关闭失败仍保留 owner 和额度。MRPC、FFI envelope 和
Endpoint framing 的身份由各自源码维护，不在手写文档复制字段表。

## 缓存与性能

持久缓存通过 `MINIGO_CACHE` 设置，用户配置见[编译缓存](USAGE.md#编译缓存)。常用诊断：

```bash
go run ./cmd/mini-go cache inspect
go run ./cmd/mini-go cache verify
MINIGO_DEBUG=cachetrace=1 go run ./cmd/mini-go check main.mgo
MINIGO_DEBUG=cachehash=1,cacheverify=1 go run ./cmd/mini-go check main.mgo
```

`cacheverify` 在命中后重建并比较结果。日常验证保留缓存；`make cache-clean` 清理 Mini-Go 缓存，
`make clean` 还清理根 `bin/`、`.cache/`、`build/` 和 Go test/fuzz 缓存。Rust target、node_modules、
dist 与本地生成镜像按所属工具单独管理。

性能测量使用 Go benchmark/pprof 或 `make runtime-rust-bench`。固定源码、输入、结果、编译参数和并行度，
串行保留多次采样；消融一次只改变一个机制，并另外验证行为、计费和回收。报告区分 guest 逻辑费用、
当前存活内存、累计分配、进程资源和吞吐/延迟，不能用短时或多实例结果推断长期同实例行为。

长期运行测试先预热固定工作集，再重复调用、取消与热更新，检查 task、timer、FFI、revision、连接和内存
是否稳定。WASM 还应记录线性内存容量。原始采样和调查结论保存在 `/tmp`。

## Fuzz 与自举

```bash
FUZZTIME=30s make fuzz-syntax
FUZZTIME=30s make fuzz-runtime
make bootstrap-test
```

普通测试运行固定 fuzz seeds；持续变异同时验证成功不变量和失败后的状态完整性。语法终止性使用 compiler
的确定性 limits。fuzz 默认使用一个 worker，避免多个完整 VM 或编译器实例争用内存和 CPU；资源充足时可用
`FUZZ_PARALLEL` 显式增加并行度。Go 自举的持续变异比较原生与 VM compiler 的检查结果，固定差分语料再比较
完整镜像、hash、符号和诊断；Rust 常规测试直接消费预编译镜像。

## 编辑器扩展

在 `vscode-ext/` 执行 `npm ci`、`npm test` 和 `npm run package`，生成 `/tmp/mini-go.vsix`。
grammar 由根生成流程维护，用户配置见[扩展 README](vscode-ext/README.md)。
