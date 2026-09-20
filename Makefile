.DEFAULT_GOAL := help
# 生成与消费共享产物的目标按顺序执行；Go/Cargo 自身仍可并行构建。
.NOTPARALLEL:

export GOTOOLCHAIN ?= go1.26.6

TEST_PACKAGES ?= ./...
TEST_FLAGS ?= -timeout=20m
COVERAGE_PROFILE ?= coverage.txt
COVERAGE_HTML ?= coverage.html
RACE_PACKAGES ?= ./ffi ./runtime ./rpc/... ./compiler/cache ./compiler/workspace ./compiler/service ./compiler/language ./tooling/lsp ./tooling/dap
RACE_FLAGS ?= -timeout=10m -p=1
FUZZTIME ?= 10s

golangci_lint := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2
minigo := go run ./cmd/mini-go
minigo_sources = $(shell find stdlib/src stdlib/host -type f \( -name '*.mgo' -o -name '*.mrpc' \) -print | sort) rpc/gateway/control.mrpc
syntax_fuzz_packages := ./compiler/scanner ./compiler/parser ./compiler/ast ./compiler/optimize ./compiler/workspace ./compiler ./compiler/bootstrap ./compiler/format ./compiler/analysis ./compiler/language ./tooling/mrpc
runtime_fuzz_packages := ./runtime/bytecode ./runtime ./rpc ./rpc/router ./rpc/gateway ./stdlib/... ./tooling/dap ./integrations

rust_dir := playground/runtime-rust
rust_manifest := $(rust_dir)/Cargo.toml
cargo_flags := --locked --manifest-path $(rust_manifest)
wasm_dir := $(rust_dir)/runtime-wasm
npm := npm --prefix $(wasm_dir)
wasm_fixtures := $(CURDIR)/$(rust_dir)/target/wasm-fixtures
runtime_images := testdata/runtime/execution.json.gz testdata/runtime/stdlib.json.gz
compiler_image := $(rust_dir)/tooling/assets/compiler.json.gz

.PHONY: help
help: ## 显示常用目标与说明
	@awk 'BEGIN { print "用法：make <目标> [变量=值]\n" } /^[a-zA-Z][a-zA-Z0-9_-]*:.*## / { split($$0, parts, ":"); sub(/^.*## /, ""); printf "  %-32s %s\n", parts[1], $$0 }' $(MAKEFILE_LIST)

# 生成入口与按需镜像
.PHONY: generate doc runtime-artifacts runtime-compiler-image _compiler-identity

generate: ## 更新全部绑定、契约、镜像与 manifest
	@go generate .

doc: ## 从源码注释生成 API 文档
	@$(minigo) doc -out docs/reference std

runtime-artifacts: $(runtime_images) $(compiler_image) ## 补齐 Rust/WASM 使用的三份本地镜像

runtime-compiler-image: $(compiler_image) ## 补齐 compiler tooling 内嵌镜像

$(runtime_images) $(compiler_image): | _compiler-identity

testdata/runtime/execution.json.gz:
	@go generate -run 'runtime-vectors ' .

testdata/runtime/stdlib.json.gz:
	@go generate -run 'runtime-stdlib-vectors ' .

$(compiler_image):
	@go generate -run 'mini-go-dev bootstrap ' .

_compiler-identity:
	@go generate -run compiler-identity .

# Go 构建、测试与质量检查
.PHONY: build test coverage race bootstrap-test chaos-syntax fmt lint

build: _compiler-identity ## 构建 Go 包及 bin/ 下的命令
	@go build ./...
	@go build -o bin/ ./cmd/...

test: _compiler-identity $(runtime_images) ## Go 测试；支持 TEST_PACKAGES、TEST_FLAGS
	@go test $(TEST_FLAGS) $(TEST_PACKAGES)

coverage: _compiler-identity $(runtime_images) ## Go 测试覆盖率；生成 coverage.txt 与 coverage.html
	@go test $(TEST_FLAGS) -covermode=atomic -coverprofile="$(COVERAGE_PROFILE)" $(TEST_PACKAGES)
	@go tool cover -html="$(COVERAGE_PROFILE)" -o "$(COVERAGE_HTML)"
	@go tool cover -func="$(COVERAGE_PROFILE)" | awk 'END { print }'

race: _compiler-identity $(runtime_images) ## Go race 检查；支持 RACE_PACKAGES、RACE_FLAGS
	@go test -race $(RACE_FLAGS) $(RACE_PACKAGES)

bootstrap-test: _compiler-identity ## 原生与 VM compiler 自举一致性测试
	@go test -timeout=5m -count=1 ./compiler/bootstrap -run '^TestCompilerImage(CompilesAndRunsSource|MatchesNativeCorpus)$$'

chaos-syntax: _compiler-identity ## 语法边界与终止性测试
	@go test ./compiler -run '^TestSyntax' -count=1

fmt: ## 格式化 Go 与 Mini-Go 源码
	@$(golangci_lint) fmt --config .golangci.yml
	@$(minigo) fmt -w $(minigo_sources)

lint: _compiler-identity ## 检查依赖边界、格式、文档与 Go lint
	@bash scripts/check-boundaries.sh
	@$(minigo) fmt -check $(minigo_sources)
	@$(minigo) doc -out docs/reference -check std
	@$(golangci_lint) run --config .golangci.yml ./...

# 持续变异测试
.PHONY: fuzz fuzz-syntax fuzz-runtime

define run_fuzz_packages
	@set -eu; \
	packages="$$(go list $(1))"; \
	for package in $$packages; do \
		targets="$$(go test "$$package" -run '^$$' -list '^Fuzz')"; \
		for target in $$targets; do \
			case "$$target" in \
				Fuzz*) \
					printf 'fuzz %s %s\n' "$$package" "$$target"; \
					go test "$$package" -run '^$$' -fuzz "^$${target}$$" -fuzztime=$(FUZZTIME);; \
			esac; \
		done; \
	done
endef

fuzz: fuzz-syntax fuzz-runtime ## 运行两组 fuzz；支持 FUZZTIME

fuzz-syntax: _compiler-identity ## 编译器与语言工具 fuzz
	$(call run_fuzz_packages,$(syntax_fuzz_packages))

fuzz-runtime: $(runtime_images) ## VM、协议与宿主 fuzz
	$(call run_fuzz_packages,$(runtime_fuzz_packages))

# Rust 运行时与 provider
.PHONY: runtime-rust-lint runtime-rust-test runtime-rust-rpc-test runtime-rust-host-test
.PHONY: runtime-rust-conformance runtime-rust-host-conformance runtime-rust-bench

runtime-rust-lint: runtime-artifacts ## Rust 格式检查与全 feature/target Clippy
	@cargo fmt --all --manifest-path $(rust_manifest) --check
	@cargo clippy $(cargo_flags) --workspace --all-features --all-targets -- -D warnings

runtime-rust-test: runtime-artifacts ## Rust VM 测试与 release tooling 测试
	@cargo test $(cargo_flags)
	@cargo test $(cargo_flags) --release -p mini-go-tooling

runtime-rust-rpc-test: ## Rust RPC、生成绑定与 Gateway 测试
	@cargo test $(cargo_flags) --features rpc --test 'rpc_*' --test cancellation
	@cargo test $(cargo_flags) --features rpc-gateway --test rpc_gateway

runtime-rust-host-test: ## Rust 原生 Host 与清理生命周期测试
	@cargo test $(cargo_flags) --features stdlib-host --lib --test stdlib_host

runtime-rust-conformance: testdata/runtime/stdlib.json.gz ## 经 Go provider 验证 Rust 标准库一致性
	@go build -o bin/mini-go-dev ./cmd/mini-go-dev
	@MINIGO_HOST_BROKER="$(CURDIR)/bin/mini-go-dev" cargo test $(cargo_flags) --release --features host-conformance --lib --test stdlib -- --nocapture

runtime-rust-host-conformance: testdata/runtime/stdlib.json.gz ## 经 Rust provider 验证标准库一致性
	@cargo test $(cargo_flags) --release --features stdlib-host --test stdlib -- --nocapture

runtime-rust-bench: ## Rust 执行、分配与 GC 基准
	@cargo bench $(cargo_flags) --bench runtime

# Go/Rust RPC 互操作
.PHONY: test-rpc-conformance _rpc-go-peer

_rpc-go-peer: _compiler-identity
	@go build -o bin/mini-go-rpc-peer-go ./cmd/mini-go-rpc-peer-go

test-rpc-conformance: _rpc-go-peer ## Go/Rust Endpoint 与 Gateway 跨进程测试
	@cargo build $(cargo_flags) --features rpc-gateway --bin mini-go-rpc-peer-rust
	@MINIGO_RPC_GO_PEER="$(CURDIR)/bin/mini-go-rpc-peer-go" MINIGO_RPC_RUST_PEER="$(CURDIR)/$(rust_dir)/target/debug/mini-go-rpc-peer-rust" go test ./integrations -run '^TestRPC(Peer|Gateway)Conformance$$' -count=1 -timeout=3m

# WASM SDK 与编译器工具
.PHONY: runtime-wasm-build runtime-wasm-test runtime-wasm-pack runtime-compiler-test _npm-deps

_npm-deps:
	@$(npm) ci

runtime-wasm-build: _npm-deps ## 构建 TypeScript、Worker、WASM 与 compiler 分发
	@$(npm) run build

runtime-wasm-test: runtime-wasm-build testdata/runtime/execution.json.gz _rpc-go-peer ## SDK 类型、格式、浏览器/Node 与安装包测试
	@$(npm) run lint
	@MINIGO_WASM_FIXTURES="$(wasm_fixtures)" cargo test $(cargo_flags) --test wasm_driver
	@MINIGO_WASM_FIXTURES="$(wasm_fixtures)" MINIGO_RPC_GO_PEER="$(CURDIR)/bin/mini-go-rpc-peer-go" $(npm) test

runtime-compiler-test: runtime-wasm-build ## Rust、Node 与 Chromium 编译器/语言工具测试
	@cargo test $(cargo_flags) --release -p mini-go-tooling --test compiler_entry --test compiler
	@cd $(wasm_dir) && node --test tests/compiler.test.js tests/tools.test.js tests/tools_fault.test.js

runtime-wasm-pack: _npm-deps ## 通过 prepack 构建 npm tarball
	@cd $(wasm_dir) && npm pack

# 本地清理
.PHONY: cache-clean clean

cache-clean: ## 清理 Mini-Go 编译缓存
	@$(minigo) cache clean

clean: cache-clean ## 清理构建、覆盖率与 Go test/fuzz 缓存
	@$(RM) -r bin .cache build coverage.txt coverage.html
	@go clean -testcache -fuzzcache
