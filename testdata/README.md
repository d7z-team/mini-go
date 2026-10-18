# 共享测试数据

本目录保存多个组件或语言共同消费的输入、预期和派生镜像。单个包专有的数据留在所属包，跨 compiler/runtime
的行为由 [integrations](../integrations/README.md) 组织。

## 目录 owner

| 路径 | 内容 |
| --- | --- |
| [syntax](syntax/) | lexer、parser、formatter 和 fuzz 使用的源码语料 |
| [runtime](runtime/) | 执行源码、bytecode 输入、步数、内存状态、wire 和多后端镜像 |
| [rpc](rpc/) | MRPC schema、wire golden、场景、服务源码、生成绑定和 VM 镜像 |
| [stdlib-host](stdlib-host/) | 宿主标准库场景与 Go/Rust 对照镜像 |
| [language](language/) | 工作区、语言查询和 compiler 工作负载 |
| [workspace/sources.json](workspace/sources.json) | 源码树、包身份、URI、资源和来源冲突 |
| [debug](debug/) | 源码断点和停止原因 |

## 事实源与派生物

runtime 数据的 owner 分工如下：

| 数据 | owner / 消费方 |
| --- | --- |
| `source/`、`execution_expected.json` | 手写源码与语义预期；Go、Rust、TypeScript 消费 |
| `memory_*.json`、`storage_cost.json`、`step_limits.json` | 手写执行和存储契约；各后端按契约验证 |
| `execution.json.gz`、`state.json`、`stdlib.json.gz` | Go 工具根据源码和预期生成的执行镜像 |
| `wire.json` | Go canonical bytecode model 生成，Rust 和 TypeScript 独立验证 |

RPC 目录中的 `schema/`、`source/`、`wire/` 和 `scenarios/` 是协议事实源；`generated/` 与 `images/` 是由
schema、源码和场景生成或编译的消费数据。`tls/` 仅提供本地测试凭据。各领域的 `manifest.json` 记录版本、
输入、owner 和派生文件身份。

## 更新

修改手写源码、schema、场景或预期后，通过根 [generate.go](../generate.go) 重新生成对应的 binding、契约、
镜像和 manifest。生成顺序、补齐镜像的入口和跨语言验证矩阵统一见
[开发指南](../DEVELOPMENT.md#生成与派生物)。
