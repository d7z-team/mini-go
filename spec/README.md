# 工具链契约

以下描述由源码生成，与对应的 Mini-Go 工具链配套使用：

| 文件 | 事实来源 |
| --- | --- |
| [bytecode.json](bytecode.json) | [runtime/bytecode](../runtime/bytecode/) 的字节码模型与校验规则 |
| [tools.json](tools.json) | [compilerentry](../compiler/bootstrap/compilerentry/) 的工具接口 DTO |

契约身份和版本由所属实现维护。修改模型后的生成流程见
[开发指南](../DEVELOPMENT.md#生成与派生物)，组件职责见[架构](../ARCHITECTURE.md)。
