# 开发指南

本文集中说明维护流程、生成、验证与诊断。公共接入见[使用指南](USAGE.md)和[RPC 指南](RPC.md)，
组件边界见[架构](ARCHITECTURE.md)，日常修改约定见 [AGENTS.md](AGENTS.md)。

[日常工作流](#日常工作流) · [生成](#生成与派生物) · [测试](#测试组织) ·
[Rust](#rust-验证) · [WASM](#wasm-与-typescript) · [性能](#缓存与性能) ·
[协议](#协议维护) · [发布](#rust-与-npm-发布) · [编辑器](#编辑器扩展)

## 开发环境

Go 工具链由 [Makefile](Makefile) 的 `GOTOOLCHAIN` 固定；Rust 最低版本见
[Cargo.toml](playground/runtime-rust/Cargo.toml)，Node.js 版本见 SDK 的
[package.json](playground/runtime-rust/runtime-wasm/package.json)。依赖由各语言 lockfile 固定。

完整生成需要 Go、make 和 rustfmt。Rust/WASM 开发还需要 Cargo、Node.js、
`wasm32-unknown-unknown` target，以及与 Cargo.lock 匹配的 wasm-bindgen-cli。

## 日常工作流

在仓库根运行 `make help` 查看目标。先验证行为所属包，再按改动范围扩大检查：

```bash
make test TEST_PACKAGES='./compiler/semantic' TEST_FLAGS='-run TestName'
make test TEST_PACKAGES='./rpc/... ./integrations'
make race RACE_PACKAGES='./rpc/...'
make lint test build
```

`TEST_FLAGS` 替换默认 Go 测试参数；`-count=1` 只跳过 Go 测试结果缓存，Mini-Go 编译缓存仍可复用。
`make coverage` 使用相同包与参数设置，输出 `coverage.txt` 和 `coverage.html`。

| 改动范围 | 最低验证 |
| --- | --- |
| Go 实现或共享行为 | 目标测试；`make lint test build` |
| 并发、取消或资源生命周期 | 取消、关闭与失败回收；Go 对应包 race、Rust owner/GC、SDK Worker 生命周期 |
| compiler、stdlib、schema 或 generator | `make generate` 后运行受影响测试 |
| stdlib API 或源码注释 | `make doc` 和相关测试 |
| Rust 原生实现 | 目标 Cargo 测试、`make runtime-rust-lint`；共享 VM 行为加跑 `make runtime-rust-test`，相关 feature 按下文补测 |
| TypeScript / WASM | `make runtime-wasm-test`，以及目标配置的 Rust Clippy |
| RPC 跨语言契约 | 两侧测试和 `make test-rpc-conformance` |
| 手写文档 | 链接、章节锚点、命令和示例；不运行生成或全量测试 |

生成和构建完成后再运行消费产物的测试。性能采样及严格超时测试与重型构建分开执行。
受限环境可设置 `GOFLAGS=-p=1`、`GOMAXPROCS`、`CARGO_BUILD_JOBS` 和 `RUST_TEST_THREADS`。
交付前运行 `git diff --check`，核对生成输入与产物，并记录实际验证结果。

### CI 变更判断

[changes.yml](.github/workflows/changes.yml) 为 Go、Rust 与语法 fuzz 判断改动范围。
PR 对比合并基点，其他事件对比当前历史上最近一次成功运行；没有可靠基线时执行完整检查。
因此，失败后的文档提交仍可能触发代码验证。

分类规则及测试位于 [.github/scripts](.github/scripts/)，运行 `make ci-script-test`。
本地边界检查、打包和版本脚本位于 [scripts](scripts/)，独立于 GitHub 环境。

## 生成与派生物

根 [generate.go](generate.go) 是统一生成入口：

| 事实源 | 派生物 |
| --- | --- |
| `.mrpc` 声明 | Go、Mini-Go、Rust、TypeScript binding |
| 类型模型 | compiler 身份编码、bytecode JSON 编解码 |
| compiler、bytecode 与标准库源码 | bootstrap bundle 和工具链 identity |
| bytecode 模型与工具 DTO | [spec](spec/README.md) 契约和 Rust 描述 |
| 共享源码与行为预期 | 预编译镜像、差分数据和 manifest |
| compiler 词法与类型事实 | VS Code grammar |

```bash
make generate
make doc
```

`make doc` 单独从标准库注释生成 `docs/reference/`。修改 schema、envelope 或缓存格式时，
同步更新所属 version/domain；生成器重构应比较产物并验证编解码。派生物从事实源更新，
移植源码保留版权与许可证。

opcode 的输入输出数量以 [opcode_contract.go](runtime/bytecode/opcode_contract.go) 为事实源。
新增 opcode 时同步声明数量规则，生成 Rust 契约，并测试有效和无效操作数。

独立运行消费方测试前，可补齐缺失的本地镜像：

```bash
make runtime-artifacts       # runtime、stdlib 和 compiler 镜像
make runtime-compiler-image  # 仅 compiler 镜像
```

这两个目标只补齐缺失文件；修改输入后使用 `make generate` 更新已有产物。
手写文档按 [文档职责](AGENTS.md#文档职责) 维护；调查、设计、实验和实施记录保存在 `/tmp`。

### 生成预编译示例

在仓库根将源码编译为 Rust/WASM 可加载的镜像：

```bash
go run ./cmd/mini-go-dev runtime-blocks -out /tmp/blocks path/to/block.mgo
```

使用同一工具链的 runtime 加载输出。仓库的 `examples/blocks` 由统一生成流程更新。

## 测试组织

| 行为 | 测试位置 |
| --- | --- |
| 单个 Go 包或 Rust 模块 | 所属包或模块 |
| 标准库行为 | 源码同目录的 `*_test.mgo` |
| 根 facade API | 根目录 |
| 跨 compiler/runtime 集成 | [integrations](integrations/README.md) |
| 多后端共同消费的输入和预期 | [testdata](testdata/README.md) |

测试验证结果、诊断、协议、缓存不变量和资源终态，并回收实例、连接、后台任务与子进程。
runtime 状态机优先使用最小 bytecode Program 和精确 `PollSteps`；跨层语义再编译源码。
复杂构造集中在所属测试辅助文件，跨语言命令由 Makefile 编排。

重型 compiler、stdlib 和集成测试复用默认缓存或 `MINIGO_CACHE`；
缓存契约、故障注入和阶段单元测试使用隔离 backend。

### Rust 验证

安装与 feature 见 [Rust README](playground/runtime-rust/README.md)，
API 见 [Rust 使用指南](playground/runtime-rust/USAGE.md)。

| 命令 | 范围 |
| --- | --- |
| `make runtime-rust-lint` | rustfmt、workspace 与相关 feature/target 的 Clippy |
| `make runtime-rust-test` | 默认 VM 测试和 release 编译器/LSP/DAP 测试 |
| `make runtime-rust-rpc-test` | RPC、生成 binding 和 Gateway |
| `make runtime-rust-host-test` | 原生 Host 与清理生命周期 |
| `make runtime-rust-host-conformance` | Rust provider 执行标准库镜像 |
| `make runtime-rust-conformance` | Go provider 经 broker 执行同一镜像 |
| `make test-rpc-conformance` | Go/Rust Endpoint 与 Gateway 互操作 |
| `make runtime-compiler-test` | Rust、Node 和 Chromium 的 compiler tooling |
| `make runtime-rust-bench` | 执行、分配和 GC 基准 |

调度、GC、热更新或帧复用改动应覆盖步骤计费、不同并行度、共享状态、等待、初始化、取消和关闭，
以及 GC/Patch/DAP 安全点。编译会话测试使用 release 模式并遵守请求期限。

`host-conformance` 是测试用 broker feature，应用宿主接入使用 `stdlib-host`。

### WASM 与 TypeScript

准备 Rust target、与 Cargo.lock 匹配的 wasm-bindgen-cli，以及测试浏览器后执行：

```bash
rustup target add wasm32-unknown-unknown
npm exec --prefix playground/runtime-rust/runtime-wasm -- playwright install chromium firefox
make runtime-wasm-test
make runtime-wasm-pack
```

`runtime-wasm-test` 构建 SDK 与分发产物，检查 TypeScript 和生成绑定，验证 codec、Worker 生命周期、
浏览器、Node、双向 RPC 与安装包。runtime 默认测试 Chromium 和 Firefox，`MINIGO_BROWSERS`
可选择引擎；tools 和安装包消费测试使用 Chromium。编译会话测试串行运行。

在 SDK 目录中可单独运行 `npm ci`、`npm run build` 和 `npm run lint`；
`npm run build -- --core` 构建不含 RPC 的版本。`WASM_BINDGEN` 可指定绑定工具，
`MINIGO_BUILD_STD=1` 使用 rust-src 和 Cargo build-std。部署见
[SDK 指南](playground/runtime-rust/runtime-wasm/README.md)。

### Fuzz 与自举

```bash
FUZZTIME=30s make fuzz-syntax
FUZZTIME=30s make fuzz-runtime
make bootstrap-test
```

普通测试运行固定 seeds；持续变异检查成功不变量和失败后的状态完整性。语法终止性由 compiler
的确定性 limits 保证。fuzz 默认单 worker，资源充足时通过 `FUZZ_PARALLEL` 调整。

Go 自举比较原生与 VM compiler 的检查结果，固定语料进一步比较镜像、hash、符号和诊断；
Rust 常规测试消费预编译镜像。

## 缓存与性能

缓存路径由 `MINIGO_CACHE` 配置，用户设置见[编译缓存](USAGE.md#编译缓存)。

### 自定义缓存

实现 `compiler/cache.Cache` 时，`StoreCompile` 接收已验证的 `cache.CompiledArtifact`。
通过 `cache.SealArtifact` 从普通产物建立快照，通过 `Copy` 获取可修改副本；`LookupCompile`
返回调用方拥有的产物。backend 生命周期由创建者管理，快照归属见[架构](ARCHITECTURE.md#缓存与快照)。

### 缓存诊断

```bash
go run ./cmd/mini-go cache inspect
go run ./cmd/mini-go cache verify
MINIGO_DEBUG=cachetrace=1 go run ./cmd/mini-go check main.mgo
MINIGO_DEBUG=cachehash=1,cacheverify=1 go run ./cmd/mini-go check main.mgo
```

`cacheverify` 在命中后重建并比较结果。`make cache-clean` 清理 Mini-Go 缓存；
`make clean` 另清理根构建目录和 Go test/fuzz 缓存。Rust target、node_modules 和 dist 按所属工具管理。
日常验证保留缓存，冷启动实验单独记录缓存状态。

### 性能测量

使用 Go benchmark/pprof 或 `make runtime-rust-bench`。固定输入、结果、构建参数、镜像身份和并行度，
串行保留多轮原始采样。消融每次只改变一个机制，功能、计费和回收单独验收。
测试二进制和 profile 输出指定到 `/tmp`，例如 `go test -c -o /tmp/mini-go-runtime.test ./runtime`。

Rust runtime bench 输出 elapsed、steps、steps/s、宿主分配与 guest heap；精确 opcode 计数单独运行。
`compiler-load` 只测加载和准备，steps 等执行指标为 `null`。测量 compiler 工作负载：

```bash
MINIGO_BENCH_COMPILER=1 cargo bench --manifest-path playground/runtime-rust/Cargo.toml --bench runtime --features compiler
```

通过 `MINIGO_BENCH_COLD=1` 测量新 VM 中的冷请求，`MINIGO_BENCH_IMAGE` 指定匹配工具链的候选镜像。
完整配置、workload 和输出字段见 [runtime benchmark](playground/runtime-rust/benches/runtime.rs)，
固定编译输入见 [language workloads](testdata/language/workloads.json)。

比较时区分加载、初始化、首次编译、缓存命中与编辑后编译；镜像携带源码不代表已完成分析。
VM 调度还须分别测量底层循环与公开 Instance，浏览器另计 Worker 往返。
采样插桩耗时与普通吞吐分开，失败或预算耗尽的请求单列。ISA 改变后同时报告工作量、指令数与耗时，
不能只比较 steps/s。

### 观测口径

| 指标 | 含义 |
| --- | --- |
| scope steps | 实际执行的 guest 指令总数 |
| 有界 PC profile | 定位函数、revision 和源码热点；检查丢弃样本数 |
| 缓存 Bytes | AST、类型、镜像和符号的保守容量估算 |
| guest live bytes / heap | 逻辑存储；结合最近盘点与其后分配理解 |
| 累计分配与 GC 统计 | 分配压力、扫描成本和回收收益 |
| RSS / WASM 线性内存 | 宿主资源，独立于 guest 计费 |

长期实验先预热，再重复调用、取消与热更新，观察 task、timer、FFI、revision、连接和内存是否稳定。
原始数据与调查结论保存在 `/tmp`。

## 协议维护

### RPC 实现维护

公共用法见 [RPC 指南](RPC.md)，共享输入见 [RPC 一致性数据](testdata/README.md#rpc-一致性数据)。
按改动 owner 选择检查重点：

| 变更 | 核对重点 |
| --- | --- |
| schema / generator | 类型、命名、契约身份、四语言 codec 和原子输出 |
| FFI / Result | 接收与丢弃、取消、迟到回复和未交付资源 |
| Endpoint | 消息顺序、背压、租约、分片与断线终态 |
| Router / publication | 路由快照、原子替换、旧绑定和关闭责任 |
| TypeScript RPC SDK | Worker、结果交付、provider 清理与平台一致性 |

协议字段与版本以所属模型及生成契约为准，测试同时覆盖正常交付和失败后的最终清理。

### 编译会话驱动

通常使用 [Rust 异步 API](playground/runtime-rust/USAGE.md#本地源码与编译器工具)或
[TypeScript tools](playground/runtime-rust/runtime-wasm/README.md#编译器与语言工具)。

自建事件循环通过 `CompilerSession` 的 `start`、`poll`、`cancel` 和 `close_now` 推进会话。
成功结果交付后调用 `acknowledge`，未交付结果调用 `abandon`；恢复只重建已确认输入，替换 owner
使用递增 generation。恢复和分析共用请求剩余期限，相关测试验证快照失效与旧 owner 清理。

响应将 metadata 与 image/symbols 字节分段编码。使用
`compilerentry.DecodeToolsResponse` 或 Rust `compiler::decode_tools_response` 解码；
格式事实源见 [compilerentry](compiler/bootstrap/compilerentry/) 和 [工具契约](spec/README.md)。

## Rust 与 npm 发布

crate 与 npm 包采用同一提交快照版本 `0.0.<git commit count>-git.g<七位 commit ID>`。
源码 manifest 使用 `0.0.0-dev`；发布脚本从完整 Git 历史派生版本，在隔离 staging 中打包。
正式发布要求完整历史与干净工作区。

```bash
make release-script-test
make release-package
make release-verify
```

`release-package` 在 `build/release/` 生成 crate 和 npm tarball；`release-verify` 解包、核对镜像，
并通过独立 Rust、Node 和 Chromium consumer 验证。调试未提交内容可设置
`RELEASE_FLAGS=--allow-dirty`，生成的 `.dirty` 版本仅供本地验证。

[publish.yml](.github/workflows/publish.yml) 在 main push 后，根据 CI 和分发验证结果发布同一提交的两端包。
准入、版本与补发规则集中在 [release-plan.mjs](.github/scripts/release-plan.mjs)。
失败后修复代码或重跑 CI，再重跑对应发布。

## 编辑器扩展

在 `vscode-ext/` 运行 `npm ci`、`npm test` 和 `npm run package`，生成 `/tmp/mini-go.vsix`。
grammar 由根生成流程维护，用户配置见[扩展 README](vscode-ext/README.md)。
