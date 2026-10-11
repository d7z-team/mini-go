# 共享测试数据

本目录保存多个组件或语言共同消费的输入与预期。包专有数据留在所属包，
跨 compiler/runtime 的源码场景归 [integrations](../integrations/README.md)。

## 数据导航

| 目录 | 用途 |
| --- | --- |
| [syntax](syntax/) | 前端、formatter 与 fuzz 的合法源码、错误恢复和预算语料 |
| [rpc](rpc/) | RPC 编码、资源场景、schema、绑定和 VM 镜像 |
| [runtime](runtime/) | 执行结果、调度、步数与内存观察 |
| [stdlib-host](stdlib-host/) | 环境、文件 I/O、console 与宿主镜像 |
| [language](language/) | 工作区、语言查询与编译器工作负载 |
| [workspace/sources.json](workspace/sources.json) | 源码树、包身份、URI、资源与来源冲突 |
| [debug](debug/) | 源码断点与停止原因 |

## 运行时一致性数据

`runtime/` 按事实 owner 与消费方分工，避免用不同宿主的分配实现互相套用：

| 输入 | 用途 | 消费方 |
| --- | --- | --- |
| `source/`、`execution_expected.json`、`execution.json.gz` | 手写结果与各优化级别执行镜像 | Go、Rust、TypeScript |
| `memory_*.json`、`state.json` | Go owner 产生的精确内存与状态观察 | Go 生成，Rust 对照 |
| `step_limits.json` | 确定性步数边界 | Go、Rust、TypeScript |
| `wire.json` | 当前严格 bytecode schema 的有效与无效输入 | Go 生成，Rust 独立解码；TypeScript 在执行镜像入口验证同一 schema |
| `stdlib.json.gz` | 标准库宿主行为镜像 | Go、Rust |

TypeScript 使用共享 contract 成本验证 live、peak、total、since-census 与释放语义，不把 JavaScript
对象大小当作 Go/Rust 的精确 byte accounting。

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

## 维护方式

1. 修改手写输入与预期，由各后端独立断言；生成观察用于记录当前工具链输出。
2. 运行 `make generate` 更新派生物和 manifest，核对来源、文件 hash 与镜像身份。
3. 运行受影响的后端测试；标准库宿主场景分别覆盖 Go 与 Rust provider。

镜像准备、测试组织与验证命令见[开发指南](../DEVELOPMENT.md#生成与派生物)。
