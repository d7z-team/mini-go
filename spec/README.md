# 工具链契约

本目录保存 compiler、runtime 和工具之间共享的生成契约。契约必须与对应的 Mini-Go 编译器和运行时版本配套。

| 文件 | 事实来源 | 消费方 |
| --- | --- | --- |
| [bytecode.json](bytecode.json) | [runtime/bytecode](../runtime/bytecode/) 的字节码模型与校验规则 | Go、Rust、TypeScript runtime |
| [tools.json](tools.json) | [compilerentry](../compiler/bootstrap/compilerentry/) 的工具接口 DTO | compiler tool、Rust 和 TypeScript tools |

契约由根 [generate.go](../generate.go) 的 `contract-spec` 和 `tools-schema` 入口生成。修改事实来源后运行
`make generate`，再按[开发指南](../DEVELOPMENT.md#生成与派生物)验证所有消费方；契约身份、版本和组件边界见
[架构说明](../ARCHITECTURE.md)。
