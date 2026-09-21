# RPC 一致性数据

Go、Rust 与 TypeScript/WASM 独立消费相同输入；手写 golden 提供协议预期，原生与跨进程测试验证实际行为。

| 路径 | 内容 |
| --- | --- |
| `wire/` | 值、FFI、Endpoint 和分片的输入与预期编码 |
| `scenarios/` | 资源操作与每步释放结果 |
| `schema/` | imports、递归、集合、数值边界和资源接口 |
| `generated/` | Go、Mini-Go、Rust、TypeScript 生成绑定 |
| `source/`、`images/` | 最小 VM 调用/服务程序及宿主预编译镜像 |
| `tls/` | 仅供本地测试的 localhost 证书和私钥 |

wire 输入用十进制字符串表达宽整数、bit pattern 表达浮点、hex 表达 bytes。各后端独立解码并
重编码，核对协议和资源生命周期。schema 派生物由 `make generate` 更新，manifest 绑定输入与镜像身份。

更新流程见[共享数据维护](../README.md#维护方式)，运行命令见[开发指南](../../DEVELOPMENT.md#rust-验证)。

TypeScript binding 使用 strict/NodeNext 配置独立编译，同一生成源码在 Browser 与 Node.js 中分别作为
client 和 provider 与 Go peer 互操作。跨进程矩阵验证 Endpoint 与 Gateway 互操作；传输、认证和失败时序
由各语言原生测试负责。
