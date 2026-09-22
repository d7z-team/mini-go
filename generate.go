package minigo

//go:generate go run ./cmd/mini-go rpc generate -rust-out playground/runtime-rust/src/stdlib_host/console_generated.rs -rust-module crate::stdlib_host::console_binding -rust-runtime crate -rust-prefix Fmt stdlib/host/console/console.mrpc
//go:generate go run ./cmd/mini-go rpc generate -rust-out playground/runtime-rust/src/stdlib_host/os_generated.rs -rust-module crate::stdlib_host::os_binding -rust-runtime crate -rust-prefix Os stdlib/host/os/types.mrpc stdlib/host/os/filesystem.mrpc stdlib/host/os/environment.mrpc

//go:generate go run ./cmd/mini-go rpc generate -go-out testdata/rpc/generated/go/types/binding.go -rust-out testdata/rpc/generated/rust/types.rs -mgo-out testdata/rpc/generated/mgo/types/binding.mgo -ts-out testdata/rpc/generated/typescript/types.ts testdata/rpc/schema/types.mrpc
//go:generate go run ./cmd/mini-go rpc generate -go-out testdata/rpc/generated/go/service/binding.go -rust-out testdata/rpc/generated/rust/service.rs -mgo-out testdata/rpc/generated/mgo/service/binding.mgo -ts-out testdata/rpc/generated/typescript/service.ts testdata/rpc/schema/service.mrpc

//go:generate go run ./cmd/mini-go-dev vscode-grammar -out vscode-ext/syntaxes/mini-go.tmLanguage.json

//go:generate go run ./cmd/mini-go rpc generate -mgo-out stdlib/src/fmt/console_mrpc_gen.mgo -mgo-package fmt -go-out stdlib/host/console/binding_gen.go -go-package console -go-prefix Fmt stdlib/host/console/console.mrpc
//go:generate go run ./cmd/mini-go rpc generate -mgo-out stdlib/src/os/os_mrpc_gen.mgo -mgo-package os -go-out stdlib/host/os/binding_gen.go -go-package oshost -go-prefix Os stdlib/host/os/types.mrpc stdlib/host/os/filesystem.mrpc stdlib/host/os/environment.mrpc
//go:generate go run ./cmd/mini-go rpc generate -go-out rpc/gateway/control_mrpc_gen.go -go-package gateway -go-prefix MiniGoGatewayControl rpc/gateway/control.mrpc
//go:generate go run ./cmd/mini-go rpc generate -rust-out playground/runtime-rust/src/rpc/control_generated.rs -rust-module crate::rpc::control -rust-runtime crate rpc/gateway/control.mrpc
//go:generate go run ./cmd/mini-go-dev compiler-core -out compiler/bootstrap/compilerentry/core_gen.go
//go:generate go run ./cmd/mini-go-dev compiler-identity -out runtime/bytecode/identity.go
//go:generate go run ./cmd/mini-go-dev tools-schema -out spec/tools.json
//go:generate go run ./cmd/mini-go-dev rpc-fixtures -root testdata/rpc
//go:generate go run ./cmd/mini-go-dev rpc-fixtures -root testdata/stdlib-host
//go:generate go run ./cmd/mini-go-dev contract-spec -out spec/bytecode.json

//go:generate go run ./cmd/mini-go-dev runtime-contract -out playground/runtime-rust/src/contract_generated.rs -vectors testdata/runtime/wire.json

//go:generate go run ./cmd/mini-go-dev runtime-vectors -out testdata/runtime/execution.json.gz

//go:generate go run ./cmd/mini-go-dev runtime-state-vectors -out testdata/runtime/state.json testdata/runtime/memory_entry.json testdata/runtime/memory_slice.json testdata/runtime/memory_channel.json testdata/runtime/memory_pointer.json testdata/runtime/memory_pointer_view.json testdata/runtime/memory_frame.json testdata/runtime/memory_zero_array.json testdata/runtime/memory_call.json testdata/runtime/memory_tail_call.json testdata/runtime/memory_initialization.json testdata/runtime/memory_root_initialization.json testdata/runtime/memory_export_initialization.json

//go:generate go run ./cmd/mini-go-dev runtime-stdlib-vectors -out testdata/runtime/stdlib.json.gz
//go:generate go run ./cmd/mini-go-dev runtime-manifest -root testdata/runtime

//go:generate go run ./cmd/mini-go-dev runtime-blocks -out playground/runtime-rust/examples/blocks playground/runtime-rust/examples/blocks/arithmetic.mgo playground/runtime-rust/examples/blocks/closure.mgo playground/runtime-rust/examples/blocks/stateful.mgo

//go:generate go run ./cmd/mini-go-dev bootstrap -out playground/runtime-rust/assets/compiler.json.gz -gzip
