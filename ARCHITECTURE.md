# 架构

本文说明组件边界、数据流和状态所有权。接入方式见[使用指南](USAGE.md)与
[RPC 指南](RPC.md)，构建、测试和性能诊断见[开发指南](DEVELOPMENT.md)。

[组件](#组件与依赖) · [编译与缓存](#编译链接与派生物) · [执行与状态](#执行模型与状态所有权) ·
[生命周期](#生命周期与宿主边界) · [工具](#语言服务与调试) · [热更新](#热更新)

## 组件与依赖

| 路径 | 职责 |
| --- | --- |
| 根包 `minigo` | 嵌入 facade：Engine、配置、源码库和入口编译 |
| `compiler/` | 解析、语义、结构化类型、HIR、优化、链接和缓存 |
| `compiler/language`、`compiler/service` | 语言查询、文档更新、分析和构建会话 |
| `runtime/bytecode/` | 字节码、执行镜像、调试符号和加载校验 |
| `runtime/` | Go VM、调度、资源限制、调试和热更新 |
| `playground/runtime-rust/` | Rust VM、原生工具和 WebAssembly SDK |
| `ffi/` | VM 与宿主之间的调用、会话和异步完成事件 |
| `rpc/`、`rpc/router/`、`rpc/gateway/` | 绑定与资源、动态路由、服务发布和传输 |
| `stdlib/`、`stdlib/host/` | 标准库源码与能力声明；可选 Go 宿主实现 |
| `tooling/` | 格式化、文档、LSP、DAP、MRPC 和一致性数据工具 |
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

compiler 拥有源码级语义，runtime 消费低级执行镜像。宿主能力通过 FFI 注入，RPC 通过 FFI 和生成
源码扩展；compiler/runtime 不依赖 RPC 实现。可复用逻辑位于 library 包，命令入口负责参数和进程
生命周期。依赖方向由 [check-boundaries.sh](scripts/check-boundaries.sh) 检查。

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

`compiler/workspace` 统一源码发现、包归属、资源和位置映射。宿主声明模块的逻辑导入前缀及源码集合，
编译器根据 import 选择依赖；CLI、LSP、DAP 与嵌入 API 共用这些规则。

根 Engine 封装检查、包编译与入口链接，返回可复用的 Program。AST 在语义处理前接受有界结构校验，
已发布的分析事实与后续改写隔离。类型语义使用 `compiler/types` 的结构化模型，文本表示用于源码、
协议和展示边界。

函数体使用带显式类型、输入、输出和释放信息的 SlotCode。优化保留求值顺序、值语义、副作用与
defer/recover 行为。Go、Rust 和 WASM 在加载时校验类型、操作数和控制流，准备执行所需元数据后
才发布 Program；源码位置通过独立的 ProgramSymbols 提供。

源码、资源和 `.mrpc` 声明是分发边界。包产物、执行镜像、绑定、缓存和 bootstrap bundle 属于当前
工具链的派生物。执行代码与调试符号具有独立身份，可以分别更新；格式与身份维护见
[生成与派生物](DEVELOPMENT.md#生成与派生物)。

### 缓存与快照

分析和构建按源码、依赖、配置及有效限制复用结果。缓存命中仍须满足本次请求的限制；外部可变输入
通过内容校验，不能沿用与当前内容不符的验证结果。对外返回的可变容器与内部快照隔离。

编译缓存保存经过验证的不可变产物快照，查询返回调用方拥有的副本。符号与导出数据绑定到对应产物；
会话与共享缓存之间的传递也保持这一所有权边界。缓存接入和诊断见[开发指南](DEVELOPMENT.md#缓存与性能)。

会话拥有增量状态，借用的缓存由创建方关闭。缓存与池具有容量或生命周期边界；请求 context、
宿主对象和凭据保持瞬态。缓存容量、guest 计费与进程内存分别观测。

## 执行模型与状态所有权

| 对象 | 所拥有的状态 |
| --- | --- |
| Engine / compiler session | 源码注册、配置、增量输入和请求协调 |
| Program | 可共享的不可变代码、类型元数据和符号 |
| Instance | globals、模块初始化、revision、调度器、限额和独占 FFI Session |
| Execution / scope | 一次入口及其派生 task、timer、FFI 调用和累计费用 |
| Task | 帧、local/临时 slot、defer、调用链和等待状态 |
| Host / Bridge / backend | 嵌入方管理和关闭的共享宿主实现 |

Go 和 Rust 使用有界执行器推进可恢复 task。`Parallelism` 限制单个实例同时推进的 task 数；
task 执行有限片段后交还执行权，单 worker 也能推进等待与多个实例。WASM 在独立 Worker 中
单线程推进。片段长度、调度公平性与 scope 的累计预算分别管理。

task 的私有帧和求值状态由唯一执行者持有；共享状态通过短事务提交。GC、热更新和需要一致视图的
调试操作在安全点执行。调度器登记运行与等待中的对象，异步宿主通过事件交付结果，不同步重入 VM。
没有可运行 task 时，实例由宿主事件或计时器唤醒。

步数按实际执行的 guest 指令计费，挂起或退出归还未使用的推进额度。内存限制统计 guest 拥有的逻辑
存储，与宿主 heap、WASM 线性内存及进程 RSS 分开。可寻址值保持稳定身份；复制、切片、指针和
反射共用值语义与别名规则。GC 覆盖帧、等待、globals、FFI 和 revision 等根。

宿主快照独立拥有数据并保留循环和别名。异步边界持有自己的输入与结果，存储复用必须等到消费者释放。

## 生命周期与宿主边界

普通入口返回只完成入口结果，其派生任务结束后 scope 才结束；main 返回会结束实例的其余任务。
取消与预算按 scope 管理，未恢复的后台 panic 会使实例失败。模块初始化由一个 task 执行，
其他调用等待同一结果。

Instance 经历 `Open → Closing → Closed`；执行故障进入 `Faulted` 后仍需关闭。关闭停止接纳、
取消工作，并等待 task、timer 和 FFI Session 收敛。关闭等待被取消只停止本次等待，已发起的清理
继续进行。共享 Host、Executor 和 backend 由宿主在实例清理后关闭。

FFI 的 Open、Start、Clock 和 Entropy 回调必须快速、线程安全。阻塞操作由宿主执行器承载，
通过 completion 返回结果。结果只能接收或丢弃一次；失败、取消和迟到回复仍有明确的资源 owner。
宿主清理在 VM 所有权之外执行，实例关闭等待其会话清理完成。

## 语言服务与调试

Go LSP 直接调用 compiler 的语言核心。会话分别管理输入 revision 与已发布 analysis snapshot，
成功分析后才替换快照，取消或失败保留上一份结果；构建使用对应版本的源码与符号。

Rust `compiler` 和 TypeScript `tools` 共用 Rust 编译会话，在 VM 中执行 Go 编译器。TypeScript
负责有界排队、Worker 监督和结果交付，编译协议、恢复与 DAP 算法由 Rust 拥有。VM 核心独立于工具层。

编译会话只恢复宿主已确认的输入。取消或交付丢失后重建会话，旧 snapshot 失效；升级在候选恢复、
分析并成功交付后切换，准备失败保留当前会话。协议维护见[编译会话驱动](DEVELOPMENT.md#编译会话驱动)。

DAP 使用 runtime 的调试接口暂停、检查和单步。符号属于代码 revision，恢复执行后帧与变量引用失效，
取消 scope 同时清理其暂停状态。

## 热更新

热更新先准备完整候选 Program，再在 Instance 安全点提交。准备检查类型、globals、导出、入口、
能力和基准 revision，失败不改变实例。提交切换当前 revision，已有帧、defer 和闭包保留旧版本；
后续命名调用进入当前版本，兼容的 global 存储保持稳定。

旧版本由实际引用保留，引用解除后回收。差异报告说明候选变化，实例负责准入与原子提交；有界引用
诊断帮助定位长期存活的旧代码。公开操作见[运行时热更新](USAGE.md#运行时热更新)。

FFI Session 和 RPC 资源跨代码 revision 存续。代码更新与服务 publication 替换分别提交；
服务替换后新绑定选择新 provider，旧绑定和资源继续由原 owner 清理。

## 标准库与 RPC

标准库纯逻辑位于 `stdlib/src`，Clock 和 Entropy 由运行环境注入，console、文件系统和环境由
provider 提供。能力声明用于宿主装配，强制能力在实例创建和补丁准备时校验。编译器依据类型化 IR
优化计算，intrinsic 承担宿主能力及 VM 基础操作。

RPC 按契约身份绑定客户端和 provider。resource 属于创建它的 binding，别名共享关闭状态；
Endpoint 管理双向操作、租约和传输，Router 管理服务选择，Publication/Gateway 管理发布和连接。
已接受的操作、结果交付与资源清理由各自 owner 负责。

Rust RPC 使用宿主 Tokio runtime，WASM 使用浏览器或 Node Worker。SDK 的 runtime 与 RPC 共用
Worker 端口、平台连接适配，业务协议和资源生命周期由各自模块管理。接入见[RPC 指南](RPC.md)，
维护见[开发指南](DEVELOPMENT.md#rpc-实现维护)。
