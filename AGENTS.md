# AGENTS.md

本文件适用于整个仓库。修改子目录前检查是否有更近的 `AGENTS.md`，其局部规则优先；
用户在当前任务中的明确要求优先于仓库约定。

项目由 Go 编译器与运行时、Rust 执行后端和 TypeScript WASM SDK 组成。
按任务阅读相关资料，不必预先遍历全部文档：

| 任务 | 入口 |
| --- | --- |
| 包边界、状态与资源归属 | [ARCHITECTURE.md](ARCHITECTURE.md) |
| 工具链、生成、测试、诊断 | [DEVELOPMENT.md](DEVELOPMENT.md) |
| Go API / CLI / RPC | [USAGE.md](USAGE.md)、[RPC.md](RPC.md) |
| Rust 原生 API | [Rust 使用指南](playground/runtime-rust/USAGE.md) |
| Browser / Node SDK | [runtime-wasm README](playground/runtime-rust/runtime-wasm/README.md) |
| 跨语言语料 | [testdata README](testdata/README.md) |

## 工作方式

- 先阅读涉及的代码、测试和文档，以实际行为确认问题，再修改拥有该行为的包。
- 开始时检查 `git status --short`，保留已有改动。未经明确要求，不创建提交、重写历史或回退无关改动。
- 大型实施计划写入 `/tmp/todo-mi.md`；设计、调查、消融候选和测试日志保存在 `/tmp`，不放入产品文档。
- 手工编辑使用 `apply_patch`；机械移动、格式化和生成使用仓库命令。
- 修改公开行为前先复现结果或 diagnostic；完成后核对实现、测试与文档一致性。

## 结构与语言规范

- 按稳定职责组织文件，不按长度机械拆分；沿用所属包的命名、错误处理和测试风格。
- 消除实际重复；单次使用且不承载独立概念的 helper 可内联。大型 opcode/AST dispatch 属于同一算法时保持集中。
- 这是 library 项目。不能仅凭仓库内无调用就删除导出 API；公共 API 调整应符合任务范围并检查使用文档。
- Go 使用 `gofmt`；Rust 使用 `cargo fmt` 和 Clippy；TypeScript 保持 strict 类型检查并使用项目 Prettier 配置。
- Rust 核对 Cargo feature 与目标平台，区分根路径的共享 `Instance` 和 `instance::Instance` 底层 VM。
  同步 VM 工作交给有界执行器，异步任务明确传递取消并等待清理；库错误沿用现有 Result/error 类型。
- TypeScript 共用 SDK 拥有协议与生命周期，browser/node 入口只适配平台。
  对外二进制输入保持调用方所有权，保留 BigInt 和图快照语义；修改 SDK 源码后重新构建分发产物。

## 架构与派生物

- 根包只负责 facade；source-level 语义由 compiler 处理，runtime 消费低级 bytecode。compiler 不导入 runtime 实现，
  runtime 不导入 compiler facade。命令入口只放在根目录 `cmd/`，可复用实现放在所属 library 包。
  按现有职责目录组织包，不新增 `internal/` 层级。
- 类型语义使用 `compiler/types` 结构化模型；canonical text 仅在 source、bytecode、RPC 和 display 边界解析。
- runtime 的宿主调用只依赖 `ffi.Bridge`，RPC 通过 FFI 与生成源码扩展；compiler/runtime 不导入 RPC/MRPC 实现。
  根 `stdlib` 只拥有源码与能力声明，`stdlib/host` 拥有 provider；依赖边界遵循 `scripts/check-boundaries.sh`。
- 正常源码输入为 `.mgo`，测试为 `_test.mgo`；bootstrap 的 Go 输入经专用适配器映射，保留原始诊断与符号路径。
  `.mrpc` 是独立生成输入，其生成的 `.mgo` 按普通源码处理。
- 模块由宿主 API 声明逻辑导入前缀和源码集合；应用与库共用 workspace 源码规则，依赖按 import 解析。
- 源码是分发边界；字节码和缓存属于当前工具链派生物。改变 schema/envelope 时同步更新所属 version/domain 与生成结果。
- 变更编译流程时同时检查 cache identity、优化级别、独立调试符号、LSP/DAP 和自举；请求 context 与宿主对象保持瞬态。

## 状态与生命周期

- 状态由明确的 owner 管理。取消、关闭和失败路径必须释放所拥有的资源；长期缓存应有边界，热更新准备失败保持原状态。
- 异步回调通过 owner event 交付结果，不同步重入 VM。优化复制或复用 buffer 时保留明确的借用、转移和释放边界，
  不让异步消费者引用会被复用的存储。
- 区分入口结果、scope 结束与实例关闭。等待被取消不必然等于工作已停止，按所属 API 的实际契约处理。

## 标准库与生成

- 标准库纯逻辑使用 `.mgo`，宿主能力通过 provider 注入。移植源码保留版权和许可证，注释描述实际支持的行为。
- generated binding、bootstrap bundle、identity 和 spec 从源码、schema 或 generator 更新，不直接修补派生文件。
  生成入口由根 `generate.go` 声明，通过 `go generate .` 或 `make generate` 执行；文档由 `make doc` 单独生成。

## 测试规范

- 测试可观察结果、diagnostic、wire shape、缓存不变量和资源生命周期。错误测试覆盖真实无效输入与失败路径，
  不为“某功能已删除”建立负向测试，也不以函数存在或数量代替行为验证。
- 数量断言只用于 ABI、数据 shape、语言求值或资源计费等明确 contract。
- 测试放在行为所属包，按 contract 命名；同一 domain 的复杂构造器集中在 `*_test_helpers_test.go`。
  stdlib 包行为写在同目录 `*_test.mgo`，Go 测试验证 host 与跨层集成。
- 无单一包 owner 的跨 compiler/runtime 集成测试统一放在 `integrations/`，按行为命名；共享辅助函数放在该目录的
  `test_helpers_test.go`，源码场景放在 `integrations/testdata/`。根目录只保留 facade 自身的 API 测试，
  各包独立拥有的测试和数据保留在所属包；共享语法语料放在 `testdata/syntax/`。
- runtime 状态机测试优先用最小 bytecode Program 和精确 `PollSteps`；确有跨层行为时才编译 Mini-Go 源码。
- Go 单元测试、benchmark 和 fuzz 使用 Go testing；Rust 使用 Cargo，SDK 使用 package.json 中的测试入口。
  跨语言命令编排留在 Makefile；Go 测试仅在验证 CLI、生成代码编译或
  跨进程行为确有需要时启动子进程，不包装整套测试命令。测试自行回收实例、连接与 goroutine。
- 重型 compiler、stdlib 和集成测试复用 `MINIGO_CACHE` 或默认磁盘缓存；cache contract、故障注入和阶段单元测试使用隔离 backend。
  日常验证不先清缓存；冷启动验证和用户要求清理时才执行 `make clean`。
- fuzz 同时验证成功不变量和失败后的状态完整性。语法终止性使用 compiler 的确定性 limits，
  不用 goroutine 超时或 `recover` 掩盖 panic。
- 共享语义优先加入 `testdata/` 所属语料并让各后端独立断言，避免多个包装测试重复运行完全相同的场景。
- 消融一次改变一个机制，保持结果与计费契约，串行采样并保留基线、配置和原始结果。
  微基准结论注明工作负载，性能数据与正确性验收分别报告。

## 验证范围

先跑目标测试，再按改动影响扩大范围。具体命令和缓存说明见 [DEVELOPMENT.md](./DEVELOPMENT.md)。

| 改动 | 验证 |
| --- | --- |
| Go 实现与测试 | 目标测试；共享行为变更执行 `make lint test build` |
| Rust 实现 | 目标 Cargo 测试、`make runtime-rust-lint`；共享 VM 行为执行 `make runtime-rust-test`，相关 feature 按开发指南补测 |
| TypeScript / WASM | `make runtime-wasm-test`；Rust WASM 代码还需目标配置的 Clippy |
| RPC 跨语言契约 | 两侧对应测试及 `make test-rpc-conformance` |
| 并发与生命周期 | 取消、关闭、失败回收；Go 补充相关 race，Rust 覆盖 owner/GC，SDK 覆盖 Worker 生命周期 |
| compiler、stdlib、schema 或 generator 输入 | 执行 `make generate` 并验证派生内容 |
| stdlib API 或源码注释 | 执行 `make doc` 与相关测试 |
| 仅手写文档 | 检查链接、命令和示例；新增示例可在 `/tmp` 编译运行，不运行仓库生成或全量测试 |

局部测试使用 `make test TEST_PACKAGES='./runtime/...' TEST_FLAGS='-run TestName'`；完整测试使用 `make test`。
集成测试使用 `make test TEST_PACKAGES='./integrations'`，也由完整测试自动发现。
Rust compiler tooling 测试使用 release 模式，遵循现有请求期限。
生成或构建结束后再启动消费对应产物的测试；性能采样和严格超时测试避免与重型构建争用资源。
失败先确认代码、产物版本和运行条件，修复或复验应保留证据，不通过放宽期限、预算或断言掩盖问题。
交付前检查 `git diff --check`；报告改动、实际验证及未完成项，只有用户要求时才创建提交。

## 文档职责

- [README.md](./README.md)、[README_zh.md](./README_zh.md)：英文/中文的项目介绍、安装、快速开始和导航。
- [USAGE.md](./USAGE.md)、[RPC.md](./RPC.md)：公开 API、CLI 与 RPC 使用方式。
- [ARCHITECTURE.md](./ARCHITECTURE.md)：包职责、依赖方向、状态机与生命周期。
- [DEVELOPMENT.md](./DEVELOPMENT.md)：开发者维护流程、生成、内部协议、测试和性能诊断。
- 组件 README 负责入门和导航，组件使用指南补充原生 API 与完整示例；通用构建命令集中在开发指南。
- 根 `README.md` 使用英文，`README_zh.md` 保存对应中文；其他手写文档使用中文。只描述当前行为，
  清理过时内容，不保留“已删除功能不应实现”一类说明。
  `docs/` 由源码注释与 `make doc` 生成，不手改；许可证和移植版权声明保持原文。
