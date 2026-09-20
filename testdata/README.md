# 共享测试数据

本目录保存多个组件或语言共同消费的输入与预期。包专有数据留在所属包，
跨 compiler/runtime 的源码场景归 [integrations](../integrations/README.md)。

## 数据导航

| 目录 | 内容与用途 |
| --- | --- |
| [syntax](syntax/) | 合法源码、错误恢复和资源预算语料；供前端、formatter 与 fuzz 使用 |
| [rpc](rpc/README.md) | wire golden、资源动作、schema、生成绑定和 VM 镜像 |
| [runtime](runtime/README.md) | 源码与独立执行预期、字节码、调度和内存观察 |
| [stdlib-host](stdlib-host/) | 环境、文件 I/O、console 场景与宿主预编译镜像 |
| [language](language/) | 共享工作区与语言查询输入 |
| [workspace/sources.json](workspace/sources.json) | 源码树、包身份、URI、二进制资源与来源冲突 |
| [debug](debug/) | 共享源码断点与停止原因预期 |

## 维护方式

手写输入与预期由各后端独立校验；生成观察只记录当前工具链输出。RPC、runtime 和 stdlib-host 的
manifest 绑定来源、文件 hash 与镜像身份。修改场景时先审阅预期，再运行 `make generate` 更新派生物，
最后执行受影响测试。

Go、Rust 和 WASM 按各自支持范围独立断言；标准库宿主场景还区分 Go 与 Rust provider。
测试组织与运行命令见[开发指南](../DEVELOPMENT.md#测试组织)，协议和运行时数据格式见对应目录说明。
