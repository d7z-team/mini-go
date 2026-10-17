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

函数体使用带显式 slot 和类型信息的 SlotCode。函数内优化保留求值顺序、值语义、副作用、
defer/recover、可寻址与捕获变量的身份，以及调试观察点。各 runtime 完成镜像校验和访问计划准备后
才发布 Program；执行元数据归所属 revision 所有，调试符号拥有独立身份。

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

### 执行存储

Go、Rust 和 TypeScript 从最终 bytecode 派生 frame 布局、引用活跃性和类型访问计划。标量和小型聚合值
保存在 frame bank；可寻址数据、较大聚合值及容器 backing 保存在受管页，以带 generation 的
数字引用定位。字段、元素、值复制和引用扫描共用预计算布局，物理布局与反射可见的语言布局分开。
Program 的布局计划保持不可变，同一模块的类型计划在加载时跨函数复用，并可由多个实例共享。
实例的动态类型目录保存独立类型闭包，存储引用和回收状态归实例所有。

同步读取可以借用类型化存储视图，赋值、参数和返回值按语言值语义完成复制。多结果写入保留平行赋值
的求值顺序，重叠复制保留源值。物理存储借用不跨越 GC 或宿主调用；长期持有者使用稳定引用。
frame 缓存按 revision 和函数隔离，复用前清空持有的引用。

可寻址值保持稳定身份；复制、切片、指针和反射共用值语义与别名规则。传给宿主的快照独立拥有数据，
并保留循环和别名。返回值、panic 值及挂起操作的输入在交付前保持可达，异步消费者拥有独立输入。

### 分配与回收

受管堆拥有逻辑存储的预留、发布、释放和累计分配账本。guest 限额覆盖已占用与预留的 payload 和
控制存储；resident 限额覆盖受管页、frame bank、目录和回收工作空间。task 与首帧一起接纳，构造失败
释放未提交预留，控制存储随 owner 释放。宿主 heap、进程 RSS 和编译缓存单独观测。

minor collection 复制年轻对象，并通过 dirty cards 查找老对象对年轻对象的引用；major collection
标记并释放整堆的不可达存储。回收只在 owner 安全点运行，native worker 先归还批量分配额度和写入记录。
根来自当前执行位置的活跃存储、指令中间值以及 continuation、FFI、iterator、revision 等实际 owner。
指令内分配保活已求值输入，目标发布后才释放来源；回收重试从挂起的分配或发布阶段继续。
scope 取消释放 task、frame 和挂起操作的 owner；失去引用的 payload 在后续回收前仍计入占用。
统计读取累计账本，上次 major 可达量作为历史快照单独呈现。步数按实际 guest 指令累计，执行位置
映射回镜像和源码，具体观测口径见[缓存与性能](DEVELOPMENT.md#缓存与性能)。

## 生命周期与宿主边界

入口结果和 scope 结束是两个事件：入口返回后，其派生 task 仍可能运行。main 返回会终止实例的其余
任务。取消和预算属于 scope；未恢复的后台 panic 会使实例失败。

Instance 的终态由实例 owner 管理。关闭停止接纳工作、取消活动 scope，并等待 task、timer 和
FFI Session 收敛；取消一次关闭等待不会撤销已经开始的清理。共享 Host、Executor、连接和 backend
在所属实例完成清理后由创建者关闭。

FFI 回调应快速返回，阻塞工作交给宿主执行器，再以 completion 交付。回复在存储接纳成功后消费，
每个回复最终接收或丢弃一次；失败、取消和迟到回复由所属 owner 清理。无法在回收后接纳回复时，
所属执行以 `allocation_limit` 结束。

## 语言服务与调试

Go LSP 直接使用 compiler 语言核心。Rust `compiler` feature 在 Rust VM 中运行编译会话；TypeScript
`tools` 在独立 Worker 中运行 Go compiler WASM。它们共用工具协议和 compiler 输出契约，各自管理
排队、期限、恢复和升级。

会话只恢复宿主已确认的输入。成功分析后才发布新 snapshot；取消、失败或升级准备失败保留当前状态。
调试符号属于代码 revision，恢复执行后帧与变量引用失效。维护入口见
[协议维护](DEVELOPMENT.md#协议维护)。

## 热更新

热更新先完整准备候选 Program，再在 Instance 安全点原子提交。准备检查类型、globals、导出、入口、
能力和基准 revision；失败不改变当前状态。已有帧、defer 和闭包继续引用旧 revision；普通命名调用
使用当前 revision，标记为 `revision_local` 的命名调用使用 caller frame 的 revision。
identity 与 layout 均匹配的 global 存储保持稳定。

旧 revision 由 code anchor 的真实引用保留，主回收确认 anchor 不可达后释放；版本目录本身不构成
guest root。共享 Program 的调用目标不保存实例的当前 revision 状态。FFI Session 与 RPC 资源跨代码
revision 存续；代码补丁和服务 publication 替换是两个独立事务。公开操作见
[热更新](USAGE.md#热更新)。

## 标准库与 RPC

标准库纯逻辑位于 `stdlib/src`；console、文件系统和环境等宿主能力由 provider 注入。
能力声明参与实例创建和补丁准入，intrinsic 只承载 VM 基础操作与通用宿主能力。

RPC 按契约身份绑定客户端与 provider。resource 属于创建它的 binding，Endpoint 管理双向操作与传输，
Router 管理服务选择，Publication/Gateway 管理发布和连接。ContractRepository 拥有不可变的 MRPC 源码
快照，整批校验成功后原子发布；发布输入和查询结果与仓库存储彼此独立。TypeScript Endpoint 同时服务
VM 内 RPC 与独立 Browser/Node RPC Worker。公开用法见 [RPC 指南](RPC.md)。
