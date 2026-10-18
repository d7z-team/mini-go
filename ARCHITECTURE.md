# 架构

本文描述组件边界、主要数据流和状态所有权。公开 API 见[使用指南](USAGE.md)与
[RPC 指南](RPC.md)，构建、生成和验证流程见[开发指南](DEVELOPMENT.md)。

## 组件边界

| 路径 | 职责 |
| --- | --- |
| 根包 `minigo` | Go 嵌入 facade：源码配置、编译入口和便捷执行 |
| `compiler/` | 解析、语义、类型、HIR、优化、链接和缓存 |
| `compiler/language`、`compiler/service` | 工作区、增量分析、语言查询和构建会话 |
| `runtime/bytecode/` | 字节码模型、执行镜像、调试符号和加载校验 |
| `runtime/` | Go VM、调度、资源限制、调试和热更新 |
| `playground/runtime-rust/` | Rust 原生 VM、源码工具、DAP 和 RPC |
| `playground/runtime-typescript/` | Browser/Node VM、Worker API、RPC 和 Go compiler WASM |
| `ffi/` | VM 与宿主之间的调用、会话和异步完成事件 |
| `rpc/`、`rpc/router/`、`rpc/gateway/` | binding 与 resource、路由、服务发布和传输 |
| `stdlib/`、`stdlib/host/` | 标准库源码与能力声明；可选 Go 宿主实现 |
| `tooling/` | 格式化、文档、LSP、DAP、MRPC 和一致性工具 |
| `cmd/` | CLI 与维护命令入口 |

```text
minigo ──> compiler ──> runtime/bytecode <── runtime ──> ffi
   └──────────────────────────────────────> runtime

stdlib/host ──> stdlib、rpc、ffi
rpc/gateway ──> rpc/router ──> rpc ──> ffi
tooling ──> compiler facts、runtime debug API
cmd ──> library packages
```

compiler 拥有源码级语义，runtime 只消费低级执行镜像。宿主调用通过 `ffi.Bridge` 注入，RPC 通过
FFI 和生成源码扩展；compiler 与 runtime 不依赖 RPC 实现。命令入口只处理参数和进程生命周期，
可复用逻辑留在所属 library 包。这些依赖方向由
[check-boundaries.sh](scripts/check-boundaries.sh) 检查。

TypeScript 的 VM、compiler 和 RPC 共用 runtime 包，Browser/Node 入口只适配平台。VM 与 Go compiler
WASM 分别运行在独立 Worker 中，调用方通过公开入口管理其生命周期。

## 编译与分发

```text
源码与资源
  → workspace 与包依赖图
  → parser / semantic / 结构化类型
  → typed HIR / 泛型特化 / 优化
  → package artifact
  → 入口链接与可达性裁剪
  → ExecutionImage + 可选 ProgramSymbols
```

`compiler/workspace` 统一包发现、逻辑导入前缀、资源和源码位置，CLI、LSP、DAP 与嵌入 API 共用该装配规则。
类型语义使用 `compiler/types` 的结构化模型；canonical text 只在源码、字节码、RPC 和展示边界解析。

各 runtime 在发布 `Program` 前完整校验镜像，并准备不可变的执行元数据。加载失败不会暴露部分结果；
调试符号具有独立身份，必须与执行镜像匹配。

源码、资源和 `.mrpc` 声明是分发边界。执行镜像、生成 binding、缓存、工具链契约和 bootstrap bundle
属于当前工具链的派生物，更新方式见[生成与派生物](DEVELOPMENT.md#生成与派生物)。

### 缓存

分析和构建缓存按源码、依赖、配置与有效限制标识。缓存保存经过验证的不可变快照；请求 context、宿主对象
和凭据不进入长期缓存。backend 生命周期由创建者管理，公开接入见[编译缓存](USAGE.md#编译缓存)，维护方式见
[缓存与性能](DEVELOPMENT.md#缓存与性能)。

## 执行状态与生命周期

| 对象 | 所拥有的状态 |
| --- | --- |
| Engine / compiler session | 源码注册、配置、增量输入和请求协调 |
| Program | 可共享的不可变代码、类型元数据和符号 |
| Instance | globals、模块初始化、revision、调度器、限额和 FFI Session |
| Execution / scope | 一次入口及其 task、timer、FFI 调用和累计费用 |
| Task | 帧、局部值、defer、调用链和等待状态 |
| Host / Bridge / backend | 嵌入方管理和关闭的共享宿主实现 |

Go 和 Rust 通过有界执行器推进可恢复 task；TypeScript VM 在实例 Worker 中单线程推进。task 私有状态由
唯一执行者持有，共享状态只在明确的安全点修改。异步宿主调用通过 owner event 返回，不同步重入 VM。

Program 保存跨实例共享的类型和布局计划，Instance 保存运行期值、受管存储和回收状态。参数、返回值、
赋值和快照遵守语言值语义及别名规则；传给宿主的快照独立拥有数据。帧与受管存储可以复用，但复用不能让
上一次执行的引用或 revision 泄漏到下一次执行。

guest 限额覆盖逻辑工作量和受管存储，resident 限额覆盖 VM 为这些状态保留的物理容量。分配、回收、取消
和热更新在 owner 控制的安全点完成；宿主 heap、进程 RSS、JavaScript heap 与编译缓存单独观测。具体指标
口径见[缓存与性能](DEVELOPMENT.md#缓存与性能)。

入口结果和 scope 结束是两个事件：入口返回后，其派生 task 仍可能继续运行；main 返回会终止实例中的其余
任务。取消和预算属于 scope，未恢复的后台 panic 会使实例失败。

关闭 Instance 会停止接纳工作、取消活动 scope，并等待 task、timer 和 FFI Session 收敛。取消一次关闭等待
不会撤销已经开始的清理。共享 Host、Executor、连接和 backend 应在使用它们的实例完成清理后由创建者关闭。

FFI 回调应快速返回。阻塞工作交给宿主执行器，完成结果再交回所属 owner；每个结果最终只接收或丢弃一次，
失败、取消和迟到结果仍由原 owner 清理。

## 语言服务与调试

Go LSP 直接使用 compiler 语言核心。Rust `compiler` feature 在 Rust VM 中运行编译会话；TypeScript
`tools` 在独立 Worker 中运行 Go compiler WASM。它们共用工具协议和 compiler 输出契约，并各自管理排队、
期限、恢复和升级。

会话只恢复宿主已确认的输入，成功分析后才发布新 snapshot；取消、失败或升级准备失败保留当前状态。
调试符号属于代码 revision，恢复执行后旧的 frame 和变量引用失效。协议维护入口见
[协议维护](DEVELOPMENT.md#协议维护)。

## 热更新

热更新先完整准备候选 Program，再在 Instance 安全点原子提交。准备会校验候选镜像、入口、能力和基准
revision；失败不改变当前状态。每次 execution 在开始时取得不可变 revision 快照，它的 frame、后续命名
调用、defer 和闭包始终使用同一 revision；补丁提交后启动的新 execution 使用新 revision。

相同 ID 且类型相同的 global 复用原存储；新增或类型变化的 global 使用新存储。旧 revision 由实际代码引用
保留，无引用后随回收释放。FFI Session 与 RPC resource 可以跨代码 revision 存续，因此代码补丁和服务
publication 替换是两个独立事务。公开操作见[热更新](USAGE.md#热更新)。

## 标准库与 RPC

标准库纯逻辑位于 `stdlib/src`；console、文件系统和环境等宿主能力由 provider 注入。能力声明参与实例创建
和补丁准入，intrinsic 只承载 VM 基础操作与通用宿主能力。

RPC 按契约身份绑定客户端与 provider。resource 属于创建它的 binding，Endpoint 管理连接与传输，Router
管理服务选择，Publication 与 Gateway 管理发布。MRPC 仓库只在整批契约校验成功后发布新快照。公开用法见
[RPC 指南](RPC.md)。
