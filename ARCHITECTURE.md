# 架构

本文说明组件职责、依赖方向、数据流和状态所有权。公共接入方式见[使用指南](USAGE.md)与
[RPC 指南](RPC.md)，生成和验证流程见[开发指南](DEVELOPMENT.md)。

## 组件与依赖

| 路径 | 职责 |
| --- | --- |
| 根包 `minigo` | 嵌入 facade：Engine、配置、源码库和入口编译 |
| `compiler/` | 解析、语义、结构化类型、HIR、优化、链接和编译缓存 |
| `compiler/language`、`compiler/service` | 语言查询、文档事务、增量分析和构建会话 |
| `runtime/bytecode/` | 字节码、执行镜像、调试符号和加载校验 |
| `runtime/` | Go VM、调度、资源限制、调试和热更新 |
| `playground/runtime-rust/` | Rust VM、原生工具链适配和 WebAssembly SDK |
| `ffi/` | VM 与宿主之间的调用、会话和异步完成事件 |
| `rpc/`、`rpc/router/`、`rpc/gateway/` | 绑定与资源、动态路由、服务发布和传输 |
| `stdlib/`、`stdlib/host/` | 标准库源码与能力声明；可选宿主 provider |
| `tooling/` | 文档、格式化、LSP、DAP、MRPC 和一致性数据工具 |
| `cmd/` | CLI 与仓库维护命令入口 |

```text
minigo ──> compiler ──> runtime/bytecode <── runtime ──> ffi
   └──────────────────────────────────────> runtime

stdlib/host ──> stdlib、rpc、ffi
rpc/router ──> rpc ──> ffi
rpc/gateway ──> rpc/router、rpc
tooling ──> compiler facts、runtime debug API
cmd ──> library packages
```

compiler 拥有源码级语义，runtime 只消费低级执行镜像；宿主能力通过 FFI 注入，RPC 通过
FFI 和生成源码扩展。compiler/runtime 不依赖 RPC 或 MRPC 实现，可复用逻辑位于 library 包，
命令入口只负责参数与进程生命周期。`scripts/check-boundaries.sh` 检查这些依赖方向。

## 编译、链接与派生物

```text
应用源码 + 标准库/注册库 + 宿主源码模块
  → SourceSet 与包依赖图
  → scanner / parser / AST
  → semantic / 结构化类型
  → 泛型特化 / typed HIR / 优化
  → package artifact
  → 入口链接与可达性裁剪
  → ExecutionImage + 可选 ProgramSymbols
```

`compiler.Check` 完成语义检查，`Compile` 生成包产物，`Prepare` 链接执行镜像；根 `Engine.Compile`
封装完整流程并返回可复用的 `Program`。源码发现、包归属和位置映射由 `compiler/workspace`
统一处理。模块身份由宿主声明的逻辑导入前缀决定，compiler 根据 import 从宿主提供的源码集合中
选择依赖。CLI、LSP、DAP 和嵌入 API 使用同一套工作区规则。

类型语义使用 `compiler/types` 的结构化模型；canonical text 只在源码、字节码、RPC 和显示边界转换。
分析与构建按源码、构建配置和依赖导出身份复用结果。优化必须保留求值顺序、捕获和副作用；链接器与
runtime 分别校验依赖、代码身份和指令，完整验证与执行准备成功后才发布 Program。

源码、资源和 `.mrpc` 声明是分发边界。package artifact、执行镜像、生成绑定、缓存和 bootstrap
bundle 都属于当前工具链的派生物，必须从事实源重新生成。调试符号与执行代码使用独立身份，
可以在代码不变时随源码位置更新。请求 context、宿主对象和凭据不进入持久缓存。

## 执行模型与状态所有权

| 对象 | 所拥有的状态 |
| --- | --- |
| Engine / compiler session | 源码注册、配置、增量输入和请求协调 |
| Program | 可跨实例共享的不可变代码、类型元数据和符号 |
| Instance | globals、模块初始化、revision、调度器、限额和独占 FFI Session |
| Execution / scope | 一次入口及其派生 task、timer、FFI 调用和累计费用 |
| Task | 执行帧、求值栈、defer、调用链和等待 continuation |
| Host / Bridge / backend | 嵌入方共享和关闭的宿主实现 |

原生 Go 和 Rust runtime 使用有界执行器承载可恢复的 task，不为 guest goroutine 创建 1:1 系统线程。
`Parallelism` 只限制同一实例可同时推进的 task 数；每个 task 执行有限 quantum 后归还 worker，
因此单 worker 仍可推进 spawn、等待和多个实例。WASM 在独立 Worker 中以相同状态机单线程推进。

task 私有的帧与求值状态在唯一执行租约下运行。channel、Mutex、模块和反射等共享状态通过短控制事务
提交；GC、热更新、完整诊断和需要一致视图的调试操作在 task 归还租约后执行。调度器单独登记 task、
scope 和等待对象，不能用它们当前是否在运行队列中判断存活。异步宿主只发布完成事件，不同步重入 VM。
Go 与 Rust 后端共享求值、步骤计费、等待和控制语义。

单次 `PollSteps` 的推进量、调度公平性和 scope 累计资源预算彼此独立。步数只按实际执行的 guest
指令计费；任务挂起或退出会归还未使用的本次额度。内存限制统计 guest 拥有的逻辑存储，和 Go/Rust
heap、WASM 线性内存或进程 RSS 分开观测。

可寻址值用稳定存储身份和路径表达，复制、切片、指针和反射操作必须保留 Mini-Go 的值语义与别名关系。
GC 从已登记的帧、等待、globals、常量、FFI 和 revision 根开始扫描。宿主快照独立拥有数据并保留循环
与别名；异步消费者不能借用将被 VM 复用的缓冲区。缓存、池和元数据必须有容量或生命周期边界。

## 生命周期与宿主边界

普通入口返回只完成入口结果，派生后台任务结束后 scope 才结束；`main` 返回会结束实例中的其余任务。
取消和预算按 scope 结算，未恢复的后台 panic 会使实例失败。模块初始化由一个 task 执行，其他调用等待
同一结果；初始化循环或失败作为统一错误传播。

Instance 生命周期为 `Open → Closing → Closed`，执行故障进入 `Faulted`，仍需关闭以释放资源。
关闭停止接纳工作、取消等待，并由实例 owner 收敛 task、timer 和 FFI Session。等待关闭所用的 context
只限制本次等待；已经发起的清理继续进行。共享 Host、Executor 和 backend 始终由创建它们的宿主关闭。

FFI 的 `Open`、`Start`、Clock 和 Entropy 回调必须快速、线程安全，也不能同步调用同一实例。
可能阻塞的宿主实现应把工作交给自己的有界执行器，再通过 completion 返回结果。每个结果只允许接收或
丢弃一次；Start 失败、执行取消和迟到回复仍由 owner 回收对应缓冲区、额度和新资源。宿主清理在 VM
所有权之外执行，但实例关闭会等待其会话完成清理。

## 语言服务与调试

Go LSP 直接调用 compiler 的语言核心。会话分别管理输入 revision 和已发布 analysis snapshot；
成功分析后才替换快照，取消或失败保留上一份结果。构建固定使用对应版本的源码和符号。

Rust `compiler` 与 TypeScript `tools` 共用 Rust 编译会话，在常驻 VM 中执行 Go 编译器。
原生异步驱动与 WASM Worker 均有限步推进；TypeScript 负责有界排队、Worker 监督和结果交付，
编译协议、恢复语义与 DAP 算法由 Rust 实现。VM 核心不反向依赖这些工具模块。

编译会话只保留宿主已确认的输入；取消或交付丢失后，从该输入重建。恢复描述内部共享不可变数据，
对外返回独立副本。升级在候选会话恢复、分析并成功交付后切换，失败保留当前会话。

DAP 通过 runtime 调试接口控制独立目标实例。`ProgramSymbols` 绑定精确代码身份，历史帧继续使用其所属
revision 的符号和源码身份；变量引用只在一次暂停期间有效，恢复执行后失效。

## 热更新

热更新先准备完整候选 Program，再在 Instance owner 的安全点提交。准备阶段检查类型、globals、导出、
入口、能力和基准 revision；失败不改变实例。提交只切换当前 revision，已有帧、defer 和闭包继续引用
旧 revision，新的命名调用进入当前 revision，兼容 global 槽保持稳定。

补丁差异只描述候选改变的代码和契约，实例负责准入与原子提交。引用诊断区分已发布 revision 和待提交
Program，以有界快照解释旧代码的保留根，不延长其寿命；引用解除后自然回收。

FFI Session 与已有 RPC 资源跨代码 revision 存续。VM patch 和服务 publication 分别提交；服务替换先发布
新 provider，新绑定选择新版本，旧绑定和资源由原 owner 清理。公开语义与诊断 API 见
[运行时热更新](USAGE.md#运行时热更新)，服务替换见[RPC 指南](RPC.md#服务替换与关闭)。

## 标准库与 RPC

标准库纯逻辑位于 `stdlib/src`。Clock 与 Entropy 由运行环境注入，console、文件系统和环境通过 provider
提供。能力声明用于宿主装配，强制能力在实例创建和补丁准备时校验。

RPC 以契约身份绑定生成客户端和 provider。operation 从请求到结果决定保持同一身份；resource 属于创建它的
binding，别名共享关闭状态。Endpoint 管理双向操作、租约、限额和传输，Router 管理一致的服务选择，
Publication/Gateway 管理发布、替换、认证和连接。取消等待、撤销授权和资源清理是不同状态，各 owner
必须继续处理已经接受的操作和清理责任。

Rust RPC 使用宿主 Tokio runtime；WASM 由浏览器 Worker 或 Node worker_threads 驱动，平台适配层只交付
网络、计时和宿主调用事件。协议使用方式见 [RPC 指南](RPC.md)，内部维护入口见
[开发指南](DEVELOPMENT.md#rpc-实现维护)。
