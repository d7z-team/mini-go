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

函数体使用带显式 slot 和类型信息的 SlotCode。优化必须保留求值顺序、值语义、副作用和
defer/recover 行为。各 runtime 完成镜像校验和元数据准备后才发布 Program；调试符号拥有独立身份。

源码、资源和 `.mrpc` 声明是分发边界。执行镜像、生成 binding、缓存、契约和 bootstrap bundle
属于当前工具链的派生物，更新方式见[生成与派生物](DEVELOPMENT.md#生成与派生物)。

### 缓存与快照

分析和构建缓存按源码、依赖、配置及有效限制标识。缓存保存经过验证的不可变快照，查询结果由调用方
拥有；符号和导出数据始终绑定到对应产物。请求 context、宿主对象和凭据不会进入长期缓存。

缓存容量、guest 计费和进程内存是不同指标。backend 生命周期由创建者管理，接入与诊断见
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

步数按实际执行的 guest 指令累计。guest 逻辑存储与宿主 heap、JavaScript heap 和进程 RSS 分开统计。
可寻址值保持稳定身份；复制、切片、指针和反射共用值语义与别名规则。传给宿主的快照独立拥有数据，
并保留循环和别名。

## 生命周期与宿主边界

入口结果和 scope 结束是两个事件：入口返回后，其派生 task 仍可能运行。main 返回会终止实例的其余
任务。取消和预算属于 scope；未恢复的后台 panic 会使实例失败。

Instance 的终态由实例 owner 管理。关闭停止接纳工作、取消活动 scope，并等待 task、timer 和
FFI Session 收敛；取消一次关闭等待不会撤销已经开始的清理。共享 Host、Executor、连接和 backend
在所属实例完成清理后由创建者关闭。

FFI 回调应快速返回。阻塞工作由宿主执行器承载，再以 completion 交付；结果只能接收或丢弃一次。
失败、取消和迟到回复都必须保留明确 owner，异步消费者不能引用已复用的存储。

## 语言服务与调试

Go LSP 直接使用 compiler 语言核心。Rust `compiler` feature 在 Rust VM 中运行编译会话；TypeScript
`tools` 在独立 Worker 中运行 Go compiler WASM。它们共用工具协议和 compiler 输出契约，各自管理
排队、期限、恢复和升级。

会话只恢复宿主已确认的输入。成功分析后才发布新 snapshot；取消、失败或升级准备失败保留当前状态。
调试符号属于代码 revision，恢复执行后帧与变量引用失效。维护入口见
[协议维护](DEVELOPMENT.md#协议维护)。

## 热更新

热更新先完整准备候选 Program，再在 Instance 安全点原子提交。准备检查类型、globals、导出、入口、
能力和基准 revision；失败不改变当前状态。已有帧、defer 和闭包继续引用旧 revision，后续命名调用
使用新 revision，兼容的 global 存储保持稳定。

旧 revision 由真实引用保留，引用解除后回收。FFI Session 与 RPC 资源跨代码 revision 存续；代码补丁
和服务 publication 替换是两个独立事务。公开操作见[运行时热更新](USAGE.md#运行时热更新)。

## 标准库与 RPC

标准库纯逻辑位于 `stdlib/src`；console、文件系统和环境等宿主能力由 provider 注入。
能力声明参与实例创建和补丁准入，intrinsic 只承载 VM 基础操作与通用宿主能力。

RPC 按契约身份绑定客户端与 provider。resource 属于创建它的 binding，Endpoint 管理双向操作与传输，
Router 管理服务选择，Publication/Gateway 管理发布和连接。TypeScript Endpoint 同时服务 VM 内 RPC
与独立 Browser/Node RPC Worker。公开用法见 [RPC 指南](RPC.md)。
