# Mini-Go Rust 编译器工具

`mini-go-tooling` 在 Rust 应用中提供 Mini-Go 编译会话、语言服务、LSP 与 DAP 适配器。
crate 内嵌与当前版本匹配的 compiler 镜像，`CompilerSession::bundled()` 可直接创建会话，无需部署额外资源。

原生 API、源码装配和关闭约定见
[Rust 使用指南](https://github.com/d7z-team/go-mini/blob/main/playground/runtime-rust/USAGE.md#本地源码与编译器工具)。
仓库构建、测试和发布流程见
[开发指南](https://github.com/d7z-team/go-mini/blob/main/DEVELOPMENT.md)。
