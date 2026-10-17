# Mini-Go 标准库

本目录保存与引擎版本配套的标准库源码、能力声明和可选 Go 宿主实现。公开包和 API 以
[生成参考](../docs/reference/README.md)为准，宿主能力的装配方式见[使用指南](../USAGE.md#宿主能力)。

## 目录职责

| 路径 | owner |
| --- | --- |
| `src/<import-path>/` | Mini-Go 标准库源码、注释和同目录 `*_test.mgo` 行为测试 |
| `host/` | console、环境和文件系统的 schema、生成绑定、provider 与 backend |
| `stdlib.go`、`capability.go` | 源码入口、能力声明和宿主装配 |

标准库源码定义 source-level 行为；Go 测试验证宿主实现和跨层结果。修改源码、能力声明或生成输入后，按
[开发指南](../DEVELOPMENT.md#生成与派生物)的流程更新派生物，再运行受影响的 `./stdlib/...` 测试。

移植源码保留原版权头，适用 [Go 许可证](LICENSE_GO)。
