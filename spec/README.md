# 工具链契约

本目录保存由源码生成的工具链契约。契约文件必须与对应的 Mini-Go 编译器和运行时版本配套：

| 文件 | 事实来源 |
| --- | --- |
| [bytecode.json](bytecode.json) | [runtime/bytecode](../runtime/bytecode/) 的字节码模型与校验规则 |
| [tools.json](tools.json) | [compilerentry](../compiler/bootstrap/compilerentry/) 的工具接口 DTO |

契约身份和版本由所属实现维护。修改模型后按[开发指南](../DEVELOPMENT.md#生成与派生物)重新生成，
并按[架构](../ARCHITECTURE.md)检查组件边界。
