# 架构

本文描述组件边界、数据流和状态所有权。公开 API 见[使用指南](USAGE.md)与
[RPC 指南](RPC.md)，构建和验证见[开发指南](DEVELOPMENT.md)。

## 组件与依赖

| 路径 | 职责 |
| --- | --- |
| 根包 `minigo` | Go 嵌入 facade：源码、Engine 配置、入口编译 |
| `compiler/` | 解析、语义、类型、HIR、优化、链接和缓存 |
| `compiler/language`、`compiler/service` | 工作区、增量分析、语言查询和构建会话 |
| `runtime/bytecode/` | 字节码、执行镜像、调试符号和加载校验 |
| `runtime/` | Go VM、调度、限制、调试和热更新 |
| `playground/runtime-rust/` | Rust 原生 VM、编译工具、DAP 和 RPC |
| `playground/runtime-typescript/` | Browser/Node VM、Worker API、RPC 和 Go compiler WASM |
| `ffi/` | VM 与宿主之间的调用、会话和异步完成事件 |
| `rpc/`、`rpc/router/`、`rpc/gateway/` | 绑定与资源、路由、服务发布和传输 |
| `stdlib/`、`stdlib/host/` | 标准库源码与能力声明；可选 Go 宿主实现 |
| `tooling/` | 格式化、文档、LSP、DAP、MRPC 和一致性工具 |
| `cmd/` | CLI 和维护命令入口 |

```text
minigo ──> compiler ──> runtime/bytecode <── runtime ──> ffi
   └──────────────────────────────────────> runtime

stdlib/host ──> stdlib、rpc、ffi
rpc/gateway ──> rpc/router ──> rpc ──> ffi
tooling ──> compiler facts、runtime debug API
cmd ──> library packages
```

compiler 拥有源码级语义，runtime 只消费低级执行镜像。宿主能力通过 `ffi.Bridge` 注入，RPC 通过
FFI 与生成源码扩展；compiler/runtime 不依赖 RPC 实现。命令入口只管理参数和进程生命周期，
可复用逻辑留在所属 library 包。依赖方向由 [check-boundaries.sh](scripts/check-boundaries.sh) 检查。

TypeScript 的 VM、compiler 与 RPC 按职责共用源码，Browser/Node 入口只适配平台。VM 与
Go compiler WASM 分别运行在独立 Worker 中；构建和部署见 [TypeScript README](playground/runtime-typescript/README.md)。

## 编译、链接与派生物

```text
源码与资源
  → workspace 与包依赖图
  → parser / semantic / 结构化类型
  → typed HIR / 泛型特化 / 优化
  → package artifact
  → 入口链接与可达性裁剪
  → ExecutionImage + 可选 ProgramSymbols
```

`compiler/workspace` 统一包发现、逻辑导入前缀、资源和源码位置。CLI、LSP、DAP 与嵌入 API
共用这一装配规则。类型语义使用 `compiler/types` 的结构化模型；canonical text 只出现在源码、
字节码、RPC 和展示边界。

函数体使用带显式 slot 和类型信息的 SlotCode。优化保留求值顺序、值语义、副作用、defer/recover
和调试观察点。各 runtime 完成镜像校验和元数据准备后才发布 Program；执行元数据归所属 revision
所有，调试符号拥有独立身份。

加载后的访问计划固定操作数、结果位置、类型布局和局部索引。私有 local 可以直接参与计算，调用和
map 迭代的多个结果可分别写入私有 local 或临时槽；可寻址、捕获、命名结果和需要重新绑定的变量保持 cell 身份。
编译器只在原值的求值时点与后续读取等价时转发 local，源位置和作用域映射随指令改写更新。

完整优化级别在函数内进行常量与副本传播，并使用控制流图和位集合计算局部变量活跃性。
可寻址和捕获变量保留存储身份；传播不跨函数或 revision。`min`/`max` 在 HIR 中保留类型和
参数顺序，生成直接消费 slot 的指令，由各后端执行相同的数值与字符串语义。

源码、资源和 `.mrpc` 声明是分发边界。执行镜像、生成 binding、缓存、契约和 bootstrap bundle
属于当前工具链的派生物，更新方式见[生成与派生物](DEVELOPMENT.md#生成与派生物)。

### 缓存与快照

分析和构建缓存按源码、依赖、配置及有效限制标识。缓存保存经过验证的不可变快照，查询结果由调用方
拥有；符号和导出数据始终绑定到对应产物。请求 context、宿主对象和凭据不会进入长期缓存。

backend 生命周期由创建者管理，接入见[编译缓存](USAGE.md#编译缓存)，诊断见
[缓存与性能](DEVELOPMENT.md#缓存与性能)。

## 执行模型与状态所有权

| 对象 | 所拥有的状态 |
| --- | --- |
| Engine / compiler session | 源码注册、配置、增量输入和请求协调 |
| Program | 可共享的不可变代码、类型元数据和符号 |
| Instance | globals、模块初始化、revision、调度器、限额和 FFI Session |
| Execution / scope | 一次入口及其 task、timer、FFI 调用和累计费用 |
| Task | 帧、slot、defer、调用链和等待状态 |
| Host / Bridge / backend | 嵌入方管理和关闭的共享宿主实现 |

Go 和 Rust 通过有界执行器推进可恢复 task；TypeScript VM 在实例 Worker 中单线程推进。
task 私有帧由唯一执行者持有，共享状态以短事务提交。GC、热更新和一致性调试操作只在安全点运行。
异步宿主调用通过 owner event 返回，不能同步重入 VM。

三端在启动入口或派生任务时，将 task 与首帧的预算一起接纳，失败的帧准备不能绕过后续计费。

### 执行存储

Go、Rust 和 TypeScript 从最终 bytecode 派生 frame 布局、引用活跃性和类型访问计划。标量和小型聚合值
保存在 frame bank；可寻址数据、较大聚合值及容器 backing 保存在受管页，以带 generation 的
数字引用定位。字段、元素、值复制和引用扫描共用预计算布局，物理布局与反射可见的语言布局分开。
Program 的布局计划保持不可变，同一模块的类型计划在加载时跨函数复用，并可由多个实例共享；
实例的动态类型目录保存独立类型闭包，
其存储引用和回收状态不写回共享计划。
操作数中的聚合值携带类型化存储视图。同步读取借用已有位置，目标存储完成必要的语言值复制；
参数、返回值和 local 共用这一拥有权边界。多结果写入保留平行赋值的求值顺序，重叠复制保留源值。
Rust 的独立值读取保存连续数据快照，已知布局的复制可在保护区间内直接写入目标 bank。
宿主图转换在输入输出边界完成。
返回值区域与 frame 一起接纳和复用，在 defer 执行及结果交付期间保持可达；回收 frame 时清空引用。
动态类型目录归实例所有；Rust 以写时复制共享目录快照，重复构造复用已验证的类型身份，
失去实际 owner 的类型及其构造缓存随主回收清理。frame 缓存按 revision 和函数隔离，复用已准备布局。

Go 共享存储按稳定句柄使用固定锁表。Rust 的共享读取在目录、metadata 和页锁保护下取得一致数据，
写入核对对象版本后提交。物理页借用不跨越 GC 或宿主调用；逻辑存储视图通过句柄重新定位。

可寻址值保持稳定身份；复制、切片、指针和反射共用值语义与别名规则。传给宿主的快照独立拥有数据，
并保留循环和别名。返回值与 panic 值在交付前保持可达；帧和 buffer 复用前释放其持有的值，异步
消费者不能引用已经复用的存储。

### 分配与回收

受管堆拥有逻辑存储的预留、发布、释放和累计分配账本。guest 限额同时接纳已占用与预留的存储；
resident 限额接纳受管页、frame bank、目录和回收工作空间。payload 与控制存储共用预算接纳规则；
worker 剩余额度转移给 owner 时，失败路径恢复原额度。构造失败释放未提交的 reservation，
frame、task 和挂起操作的控制存储随其 owner 释放。宿主 heap、进程 RSS 和编译缓存单独观测。

minor collection 复制年轻对象，并通过 dirty cards 查找老对象对年轻对象的引用；major collection
标记并释放整堆的不可达存储。回收只在 owner 安全点运行，native worker 先归还批量分配额度和写入记录。
根来自当前执行位置的活跃存储、指令中间值以及 continuation、FFI、iterator、revision 等实际 owner。
统计读取累计账本，不触发可达图扫描；上次 major 的可达量是历史快照，与当前占用分开呈现。
scope 取消时立即释放 task、frame 和挂起操作的 owner；失去引用的 payload 在后续自动或显式回收时
释放，回收前仍计入占用。分配挂起保存已求值输入及未发布结果，恢复时继续分配或发布阶段。

步数按实际执行的 guest 指令累计，执行 PC 与镜像位置保留映射，供调试和性能定位使用。
指令 scratch 只供当前同步执行使用，异步调用和派生任务取得独立输入。

指令内分配期间，源存储由当前 PC 的根集合保活；目标发布后才按引用计划释放源。channel、defer、
continuation、反射和 FFI 等长期持有者在交付前取得稳定存储，不引用即将复用的 frame 或 scratch。

## 生命周期与宿主边界

入口结果和 scope 结束是两个事件：入口返回后，其派生 task 仍可能运行。main 返回会终止实例的其余
任务。取消和预算属于 scope；未恢复的后台 panic 会使实例失败。

Instance 的终态由实例 owner 管理。关闭停止接纳工作、取消活动 scope，并等待 task、timer 和
FFI Session 收敛；取消一次关闭等待不会撤销已经开始的清理。共享 Host、Executor、连接和 backend
在所属实例完成清理后由创建者关闭。

FFI 回调应快速返回。阻塞工作由宿主执行器承载，再以 completion 交付。三端在接纳回复的 guest
存储后消费 completion；回收重试期间保留回复及其释放回调，只重试物化。每个回复最终接收或
丢弃一次，失败、取消和迟到回复由所属 owner 清理。宿主调用错误按 FFI 状态交付，包括启动失败；
回收后仍无法接纳回复时，以 `allocation_limit` 结束所属执行并拒绝该回复。

## 语言服务与调试

Go LSP 直接使用 compiler 语言核心。Rust `compiler` feature 在 Rust VM 中运行编译会话；TypeScript
`tools` 在独立 Worker 中运行 Go compiler WASM。它们共用工具协议和 compiler 输出契约，各自管理
排队、期限、恢复和升级。

会话只恢复宿主已确认的输入。成功分析后才发布新 snapshot；取消、失败或升级准备失败保留当前状态。
调试符号属于代码 revision，恢复执行后帧与变量引用失效。维护入口见
[协议维护](DEVELOPMENT.md#协议维护)。

Compiler Worker 的内部请求和响应通过 transfer 移交独占 buffer；恢复时从已确认的逻辑输入重新编码。
解码后的 Image 与 Symbols 各自拥有独立 buffer，公开 API 的调用方输入继续由调用方拥有。

## 热更新

热更新先完整准备候选 Program，再在 Instance 安全点原子提交。准备检查类型、globals、导出、入口、
能力和基准 revision；失败不改变当前状态。已有帧、defer 和闭包继续引用旧 revision；普通命名调用
使用当前 revision，标记为 `revision_local` 的命名调用使用 caller frame 的 revision。
identity 与 layout 均匹配的 global 存储保持稳定。

direct call 的 prepared target 属于 caller 的源 Program。当前帧可直接使用该目标；旧帧中的普通
命名调用按逻辑身份解析当前 revision，revision-local 调用则保持源目标。共享 Program 不保存
任何 Instance 的 current target，目标索引也不跨 revision 解释。

旧 revision 由 code anchor 的真实引用保留，主回收确认 anchor 不可达后释放；版本目录本身不构成
guest root。FFI Session 与 RPC 资源跨代码 revision 存续；代码补丁
和服务 publication 替换是两个独立事务。公开操作见[运行时热更新](USAGE.md#运行时热更新)。

## 标准库与 RPC

标准库纯逻辑位于 `stdlib/src`；console、文件系统和环境等宿主能力由 provider 注入。
能力声明参与实例创建和补丁准入，intrinsic 只承载 VM 基础操作与通用宿主能力。

RPC 按契约身份绑定客户端与 provider。resource 属于创建它的 binding，Endpoint 管理双向操作与传输，
Router 管理服务选择，Publication/Gateway 管理发布和连接。ContractRepository 拥有不可变的 MRPC 源码
快照，整批校验成功后原子发布；发布输入和查询结果与仓库存储彼此独立。TypeScript Endpoint 同时服务
VM 内 RPC 与独立 Browser/Node RPC Worker。公开用法见 [RPC 指南](RPC.md)。
