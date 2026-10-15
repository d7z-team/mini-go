# 开发指南

本文集中维护环境、生成、验证、诊断和发布流程。公共用法见[使用指南](USAGE.md)与
[RPC 指南](RPC.md)，组件边界见[架构](ARCHITECTURE.md)，仓库修改约定见 [AGENTS.md](AGENTS.md)。

## 开发环境

Go 版本由 [Makefile](Makefile) 的 `GOTOOLCHAIN` 固定；Rust 最低版本见
[Cargo.toml](playground/runtime-rust/Cargo.toml)，TypeScript 工具使用 Node.js 24.11 或更高版本，具体约束见
[package.json](playground/runtime-typescript/package.json)。各语言依赖由 lockfile 固定。

准备 Go、make、Cargo、rustfmt 和 Node.js 后，在仓库根安装依赖并补齐未随 Git 分发的镜像：

```bash
make deps artifacts
```

## 日常工作流

先运行行为所属包的目标测试，再按影响范围扩大验证。`make help` 列出全部入口。

```bash
make test TEST_PACKAGES='./compiler/semantic' TEST_FLAGS='-run TestName'
make test TEST_PACKAGES='./rpc/... ./integrations'
make race TEST_PACKAGES='./rpc/...'
make lint test build
```

`TEST_FLAGS` 替换默认 Go 测试参数；`-count=1` 只跳过 Go 测试结果缓存。`make check` 聚合下表中的
Go、Rust、TypeScript、互操作及扩展与脚本验证。生成或构建完成后再测试消费方。

| 改动范围 | 最低验证 |
| --- | --- |
| Go 实现或共享行为 | 目标测试；`make lint test build` |
| 并发、取消或资源生命周期 | 目标测试、失败清理；Go 对应包 race |
| compiler、stdlib、schema 或 generator | `make generate` 后运行所有消费方测试 |
| stdlib API 或源码注释 | `make doc` 和相关行为测试 |
| Rust 原生实现 | 目标 Cargo 测试；`make lint-rust test-rust` |
| TypeScript VM 或 compiler WASM | `make test-typescript`（包含类型检查与构建） |
| RPC 跨语言契约 | 两侧测试；`make test-interop` |
| 编辑器扩展 / CI 与发布脚本 | `make test-vscode` / `make test-scripts` |
| 手写文档 | 链接、锚点、命令和示例 |

性能采样和严格期限测试应避开重型构建。受限环境可设置 `GOFLAGS=-p=1`、`GOMAXPROCS`、
`CARGO_BUILD_JOBS` 和 `RUST_TEST_THREADS`。交付前运行 `git diff --check` 并记录实际验证结果。

## 生成与派生物

根 [generate.go](generate.go) 是统一事实入口：

| 事实源 | 主要派生物 |
| --- | --- |
| `.mrpc` | Go、Mini-Go、Rust、TypeScript binding |
| compiler、bytecode 与标准库源码 | bootstrap bundle、镜像和工具链 identity |
| bytecode 与工具 DTO | [工具链契约](spec/README.md)及 Rust/TypeScript 描述 |
| 共享输入与预期 | 测试镜像、差分数据和 manifest |
| 词法、类型和固定 Unicode 数据 | grammar、字符表与名称表 |

```bash
make generate
make doc
```

`make generate` 更新绑定、契约、镜像和 manifest；`make doc` 单独从标准库注释生成
`docs/reference/`。仅补齐未随 Git 分发且 identity 已匹配的镜像时使用 `make artifacts`。
派生物从事实源生成；改变 schema、envelope 或缓存格式时同步更新所属版本或 identity，并核对 Git 差异。

新增 opcode 从 [opcode_contract.go](runtime/bytecode/opcode_contract.go) 更新契约，生成各 runtime
描述，并补充共享语料的有效与无效输入。指令融合还需核对 compiler 的 slot 生命周期、三端类型校验
和调试位置映射。

### 外部参考

Unicode 与标准库参考使用仓库固定的工具链和 SHA-256。只有更新参考源时才运行：

```bash
GOTOOLCHAIN=go1.27.1 go run ./cmd/mini-go-dev core-api snapshot
GOTOOLCHAIN=go1.26.6 go run ./cmd/mini-go-dev core-api snapshot -json-v1
GOTOOLCHAIN=go1.26.6 go run ./cmd/mini-go-dev core-api verify
```

普通生成使用已固定的参考，不刷新外部基线。

### 生成预编译示例

```bash
go run ./cmd/mini-go-dev runtime-blocks -out /tmp/blocks path/to/block.mgo
```

输出可由同一工具链版本的 Go、Rust 和 TypeScript runtime 加载。仓库示例由统一生成流程维护。

## 测试组织

| 行为 | 所属位置 |
| --- | --- |
| 单个 Go 包或 Rust/TypeScript 模块 | 对应包或模块 |
| 标准库行为 | 源码同目录的 `*_test.mgo` |
| 根 facade API | 根目录 |
| 跨 compiler/runtime 行为 | [integrations](integrations/README.md) |
| 多后端共享输入和预期 | [testdata](testdata/README.md) |

测试断言可观察行为、wire shape、缓存不变量和资源终态。跨语言命令由 Makefile 编排；重型测试复用
Mini-Go 缓存，缓存契约和故障注入使用隔离 backend。完整测试约定见 [AGENTS.md](AGENTS.md#测试规范)。

### Rust 验证

`make lint-rust` 检查格式、最小/单 feature 编译和完整 Clippy；`make test-rust` 覆盖 VM、compiler、
LSP/DAP、RPC、Host 和 stdlib。`make test-interop` 验证 Go/Rust RPC 与 Go provider 标准库。

局部测试可传 `RUST_TEST_FLAGS` 或直接运行 Cargo。编译会话测试使用 `test-release` profile，
正式发布和 benchmark 使用 `release` profile。feature 选择见 [Rust README](playground/runtime-rust/README.md)。

### TypeScript runtime 与 Go compiler WASM

TypeScript VM 执行 bytecode，源码工具在独立 Worker 中运行 Go compiler WASM。首次运行浏览器测试前安装
Playwright 浏览器，此后使用根 Makefile 完成构建与验证：

```bash
make deps runtime-execution-image
npm exec --prefix playground/runtime-typescript -- playwright install chromium firefox
make test-typescript
```

`make build-typescript` 生成 Go compiler WASM，并通过 Vite 构建 Browser/Node 入口、Worker、WASM
资源和类型声明。`make test-typescript` 包含严格类型检查与 Vitest 测试，覆盖 Node、浏览器、compiler、
RPC、Worker 生命周期，以及安装包在 Vite 开发与生产模式下的使用。
`MINIGO_BROWSERS` 可筛选浏览器；`make pack-typescript` 生成 npm 包。
SDK 接入与部署见 [TypeScript README](playground/runtime-typescript/README.md)。

### Fuzz 与自举

```bash
make fuzz-list
FUZZTIME=30s make fuzz
make fuzz FUZZ_PACKAGES='./compiler/... ./tooling/...' FUZZ_PATTERN='Parse'
```

`fuzz-list` 用于确认目标和总运行量；`FUZZTIME` 按目标计费，`FUZZ_PARALLEL` 控制 worker 数。
语法终止性依赖 compiler 的确定性 limits。自举测试比较原生与 VM compiler 的诊断、镜像、hash 和符号。

## 缓存与性能

缓存路径由 `MINIGO_CACHE` 配置，用户操作和自定义 backend 接入见[编译缓存](USAGE.md#编译缓存)。

### 缓存诊断

```bash
go run ./cmd/mini-go cache inspect
go run ./cmd/mini-go cache verify
MINIGO_DEBUG=cachetrace=1 go run ./cmd/mini-go check main.mgo
MINIGO_DEBUG=cachehash=1,cacheverify=1 go run ./cmd/mini-go check main.mgo
```

`make cache-clean` 清理 Mini-Go 缓存，`make clean` 清理构建与覆盖率产物。冷启动实验使用隔离缓存；
日常验证保留缓存。

### 性能测量

使用 Go benchmark/pprof、`make bench-rust` 或 `make bench-typescript`。固定输入、结果、构建参数、镜像
identity 和并行度，串行保留多轮原始采样；消融一次只改变一个机制。VM 核心与公开 API、冷启动与缓存
命中应分别测量。联合比较 elapsed、总 steps、steps/s、opcode 和分配，因为 opcode mix 变化后 steps/s
不能单独表示改进。profile 只用于诊断，不作为性能门槛；原始结果与临时 profile 放在 `/tmp`。

TypeScript 的 `profile: true` 统计 opcode、函数调用和步数；`profileTiming: true` 增加逐步 self time
计时。宿主成本使用 CPU/heap sampling 分析；guest allocation count、JavaScript heap 差值和采样次数
均不能代表 V8 的精确对象分配次数。正确性测试验证结果与精确预算，不以执行耗时作为通过条件。

### 观测口径

| 指标 | 含义 |
| --- | --- |
| scope steps / opcode | guest 工作量与指令构成 |
| PC profile | 函数、revision 和源码热点 |
| guest live bytes / heap | guest 逻辑存储 |
| 累计分配与 GC | 分配压力和回收成本 |
| RSS / JavaScript heap / WASM 线性内存 | 宿主资源，不属于 guest 计费 |

## 协议维护

### RPC 实现维护

公共用法见 [RPC 指南](RPC.md)，共享输入见 [RPC 一致性数据](testdata/README.md#rpc-一致性数据)。

| 变更 owner | 必查内容 |
| --- | --- |
| schema / generator | 契约 identity、类型映射、四语言 codec 和原子输出 |
| FFI / result | 接收或丢弃、取消、迟到回复和未交付资源 |
| Endpoint | 顺序、背压、租约、分片和断线终态 |
| Router / publication | 路由快照、原子替换、旧 binding 和关闭责任 |
| TypeScript RPC | Worker、provider 清理和 Browser/Node 一致性 |

### 编译会话驱动

应用通常使用 [Rust 编译会话](playground/runtime-rust/USAGE.md#调试与-compiler-session)或
[TypeScript tools](playground/runtime-typescript/README.md#源码工具与调试)。自建驱动必须在成功交付后
确认状态，未交付结果应丢弃；恢复只重放已经确认的输入，升级准备失败保留当前 owner。
字段和版本以 [compilerentry](compiler/bootstrap/compilerentry/)及[工具链契约](spec/README.md)为准。

## CI 与发布

[publish.yml](.github/workflows/publish.yml) 编排验证和发布，[changes.yml](.github/workflows/changes.yml)
决定改动范围。修改 CI、打包或发布脚本后运行 `make test-scripts`。

Rust crate 与 npm 包使用同一提交快照版本。打包要求完整 Git 历史和干净工作区，生成的 metadata
用于核对发布产物与 registry。

```bash
make test-scripts
make release-package
make release-verify
```

产物写入 `build/release/`，验证会以独立 Rust、Node 和 Chromium consumer 解包使用。仅调试未提交内容时
可设置 `RELEASE_FLAGS=--allow-dirty`；正式发布由 main push 在验证通过后执行。

## 编辑器扩展

运行 `make test-vscode` 会在 `vscode-ext/` 执行 `npm ci`、测试和打包，生成 `/tmp/mini-go.vsix`。
grammar 由根生成流程维护，用户配置见[扩展 README](vscode-ext/README.md)。
