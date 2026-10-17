# 共享测试数据

本目录保存多个组件或语言共同消费的输入与预期。包专有数据留在所属包；跨 compiler/runtime
的行为测试由 [integrations](../integrations/README.md) 组织。

## 共享目录

| 目录 | 用途 |
| --- | --- |
| [syntax](syntax/) | 前端、formatter 与 fuzz 的合法源码、错误恢复和预算语料 |
| [rpc](rpc/) | RPC 编码、资源场景、schema、绑定和 VM 镜像 |
| [runtime](runtime/) | 执行结果、调度、步数与内存观察 |
| [stdlib-host](stdlib-host/) | 环境、文件 I/O、console 与宿主镜像 |
| [language](language/) | 工作区、语言查询与编译器工作负载 |
| [workspace/sources.json](workspace/sources.json) | 源码树、包身份、URI、资源与来源冲突 |
| [debug](debug/) | 源码断点与停止原因 |

## 运行时数据

`runtime/` 按事实 owner 与消费方分工，避免用不同宿主的分配实现互相套用：

| 输入 | 用途 | 消费方 |
| --- | --- | --- |
| `source/`、`execution_expected.json`、`execution.json.gz` | 手写结果与各优化级别执行镜像 | Go、Rust、TypeScript |
| `memory_*.json`、`state.json` | Go owner 产生的精确内存与状态观察 | Go 生成，Rust 对照 |
| `step_limits.json` | 确定性步数边界 | Go、Rust、TypeScript |
| `wire.json` | 当前严格 bytecode schema 的有效与无效输入 | Go 生成，Rust 独立解码；TypeScript 在执行镜像入口验证同一 schema |
| `stdlib.json.gz` | 标准库宿主行为镜像 | Go、Rust |

三端从共享 contract 取得逻辑存储成本；`runtime/storage_cost.json` 用独立期望核对 cell、backing、
函数、map 和 channel 的实际分配增量。测试同时验证 reservation 的提交与回滚、owner 释放和 GC 搬迁。
当前占用、尚未提交的预留、累计分配与上次 major 可达快照分别观察。resident 反映各后端受控存储的
实际容量，不要求跨后端相等；JavaScript 对象大小与进程 RSS 单独测量。

## RPC 一致性数据

`rpc/` 的手写 golden 定义协议预期，原生和跨进程测试验证实际调用与资源清理：

| 路径 | 用途 |
| --- | --- |
| `wire/` | 值、FFI、Endpoint 与分片编码 |
| `scenarios/` | 资源动作及逐步释放预期 |
| `schema/`、`generated/` | 共享声明与四语言生成绑定 |
| `source/`、`images/` | VM 调用、服务程序与预编译镜像 |
| `tls/` | 仅供本地测试的 localhost 证书和私钥 |

wire 输入使用十进制字符串表示宽整数、bit pattern 表示浮点、hex 表示 bytes，各后端独立解码并重编码。

## 更新数据

修改手写输入与预期后，运行 `make generate` 更新派生物和 manifest，再运行受影响的后端测试。
标准库宿主场景分别覆盖 Go 与 Rust provider；镜像准备和验证命令见[开发指南](../DEVELOPMENT.md#生成与派生物)。
