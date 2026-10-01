# Mini-Go 标准库

本目录保存与引擎同版本发布的标准库源码、能力声明和可选 Go 宿主实现。
公开包与 API 见 [生成参考](../docs/reference/README.md)，宿主装配见
[使用指南](../USAGE.md#系统能力)。

## 目录

| 路径 | 内容 |
| --- | --- |
| `src/<import-path>/` | Mini-Go 源码及同目录的 `*_test.mgo` 行为测试 |
| `host/` | console、环境和文件系统的 schema、生成绑定、provider 与 backend |
| 根 package `stdlib` | 不可变源码入口与能力声明 |

## 维护

公开注释与同目录行为测试描述实际支持的 API，Go 测试验证宿主和跨层集成。
生成与验证顺序见[开发指南](../DEVELOPMENT.md#生成与派生物)。

```bash
make test TEST_PACKAGES='./stdlib/...'
```

移植源码保留原版权头，适用 [Go 许可证](LICENSE_GO)。
