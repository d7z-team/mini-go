.DEFAULT_GOAL := help
# 避免共享产物构建与严格期限测试相互争用；语言工具内部仍可并行。
.NOTPARALLEL:

export GOTOOLCHAIN ?= go1.26.6

TEST_PACKAGES ?= ./...
TEST_FLAGS ?= -timeout=20m
RACE_FLAGS ?= -timeout=10m -p=1
COVERAGE_PROFILE ?= coverage.txt
COVERAGE_HTML ?= coverage.html
FUZZ_PACKAGES ?= ./...
export FUZZ_PATTERN ?= ^Fuzz
export FUZZTIME ?= 10s
export FUZZ_PARALLEL ?= 1
RUST_TEST_FLAGS ?=
TYPESCRIPT_PACK_OUTPUT ?= $(CURDIR)/build/npm
RELEASE_OUTPUT ?= $(CURDIR)/build/release
RELEASE_FLAGS ?=

golangci_lint := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2
minigo := go run ./cmd/mini-go
minigo_dev := go run ./cmd/mini-go-dev
minigo_sources = $(shell find stdlib/src stdlib/host -type f \( -name '*.mgo' -o -name '*.mrpc' \) -print | sort) rpc/gateway/control.mrpc
rust_dir := playground/runtime-rust
cargo_flags := --locked --manifest-path $(rust_dir)/Cargo.toml
rust_test_profile := test-release
typescript_dir := playground/runtime-typescript
npm := npm --prefix $(typescript_dir)
typescript_compiler_dir := $(typescript_dir)/.build/compiler
typescript_compiler_wasm := $(typescript_compiler_dir)/mini-go-compiler.wasm
runtime_execution_image := testdata/runtime/execution.json.gz
runtime_stdlib_image := testdata/runtime/stdlib.json.gz
runtime_images := $(runtime_execution_image) $(runtime_stdlib_image)
compiler_image := $(rust_dir)/assets/compiler.json.gz
.PHONY: help check generate doc artifacts runtime-images runtime-execution-image runtime-stdlib-image compiler-image _check-identity _check-runtime-images _check-execution-image _check-compiler-image _check-artifacts
help: ## 显示常用目标与说明
	@awk 'BEGIN { print "用法：make <目标> [变量=值]\n" } /^[a-zA-Z][a-zA-Z0-9_-]*:.*## / { split($$0, parts, ":"); sub(/^.*## /, ""); printf "  %-24s %s\n", parts[1], $$0 }' $(MAKEFILE_LIST)

check: lint test build lint-rust test-rust test-interop test-typescript test-vscode test-scripts ## 完整验证；先准备工具、deps 和生成产物

# generate 更新全部事实；artifacts 只在身份匹配时补齐未随 Git 分发的镜像。
generate: ## 更新全部绑定、契约、镜像与 manifest
	@go generate .

doc: ## 从源码注释生成 API 文档
	@$(minigo) doc -out docs/reference std

artifacts: runtime-images $(compiler_image) ## 显式补齐缺失的全部测试与 compiler 镜像

runtime-images: _check-identity $(runtime_images) ## 显式补齐缺失的 runtime 与 stdlib 测试镜像

runtime-execution-image: _check-identity $(runtime_execution_image) ## 补齐跨 VM execution 测试镜像

runtime-stdlib-image: _check-identity $(runtime_stdlib_image) ## 补齐 Rust 标准库测试镜像

compiler-image: _check-identity $(compiler_image) ## 显式补齐缺失的 compiler 镜像

testdata/runtime/execution.json.gz:
	@go generate -run 'runtime-vectors ' .

testdata/runtime/stdlib.json.gz:
	@go generate -run 'runtime-stdlib-vectors ' .

$(compiler_image):
	@go generate -run 'mini-go-dev bootstrap ' .

_check-identity:
	@$(minigo_dev) compiler-identity -out runtime/bytecode/identity.go -check

_check-runtime-images: _check-identity
	@for image in $(runtime_images); do test -f "$$image" || { echo "Missing $$image; run make runtime-images (or make generate after source changes)" >&2; exit 1; }; done

_check-execution-image: _check-identity
	@test -f "$(runtime_execution_image)" || { echo "Missing $(runtime_execution_image); run make runtime-execution-image" >&2; exit 1; }

_check-compiler-image: _check-identity
	@test -f "$(compiler_image)" || { echo "Missing $(compiler_image); run make compiler-image" >&2; exit 1; }

_check-artifacts: _check-runtime-images _check-compiler-image

# 常规 Go 任务由 Go testing 自动发现。
.PHONY: build test coverage race fmt lint fuzz fuzz-list
build: _check-identity ## 构建 Go 包与 bin/ 下的命令
	@go build ./...
	@go build -o bin/ ./cmd/...

test: _check-runtime-images ## Go 测试；支持 TEST_PACKAGES、TEST_FLAGS
	@go test $(TEST_FLAGS) $(TEST_PACKAGES)

coverage: _check-runtime-images ## Go 覆盖率；使用 TEST_PACKAGES、TEST_FLAGS
	@go test $(TEST_FLAGS) -covermode=atomic -coverprofile="$(COVERAGE_PROFILE)" $(TEST_PACKAGES)
	@go tool cover -html="$(COVERAGE_PROFILE)" -o "$(COVERAGE_HTML)"
	@go tool cover -func="$(COVERAGE_PROFILE)" | awk 'END { print }'

race: _check-runtime-images ## Go race；使用 TEST_PACKAGES、RACE_FLAGS
	@go test -race $(RACE_FLAGS) $(TEST_PACKAGES)

fmt: ## 格式化 Go 与 Mini-Go 源码
	@$(golangci_lint) fmt --config .golangci.yml
	@$(minigo) fmt -w $(minigo_sources)

lint: _check-identity ## Go lint、依赖边界、源码格式与生成文档检查
	@bash scripts/check-boundaries.sh
	@$(minigo) fmt -check $(minigo_sources)
	@$(minigo) doc -out docs/reference -check std
	@$(golangci_lint) run --config .golangci.yml ./...

fuzz: _check-runtime-images ## 持续变异；支持 FUZZ_PACKAGES、FUZZ_PATTERN、FUZZTIME、FUZZ_PARALLEL
	@bash scripts/fuzz.sh $(FUZZ_PACKAGES)

fuzz-list: _check-identity ## 列出所选包中匹配的 fuzz 函数
	@bash scripts/fuzz.sh --list $(FUZZ_PACKAGES)

# 原生功能测试使用完整应用 feature；外部 Go broker 单独归跨进程验证。
.PHONY: lint-rust test-rust bench-rust test-interop _rpc-go-peer
lint-rust: _check-artifacts ## Rust 格式、最小 feature 编译与完整 Clippy
	@cargo fmt --all --manifest-path $(rust_dir)/Cargo.toml --check
	@cargo check $(cargo_flags) -p mini-go --no-default-features
	@for feature in compiler dap language-server rpc rpc-gateway stdlib-host; do cargo check $(cargo_flags) -p mini-go --no-default-features --features "$$feature" || exit; done
	@cargo clippy $(cargo_flags) --workspace --all-features --all-targets -- -D warnings

test-rust: _check-artifacts ## 优化构建的原生 VM、compiler、RPC、Host 与标准库测试
	@cargo test $(cargo_flags) --profile $(rust_test_profile) -p mini-go --features language-server,rpc-gateway,stdlib-host --no-fail-fast $(RUST_TEST_FLAGS)

bench-rust: _check-runtime-images ## Rust 执行、分配与 GC 基准
	@cargo bench $(cargo_flags) --bench runtime

_rpc-go-peer: _check-identity
	@go build -o bin/mini-go-rpc-peer-go ./cmd/mini-go-rpc-peer-go

test-interop: _check-artifacts _rpc-go-peer ## Go/Rust RPC 互操作与 Go provider 标准库验证
	@go build -o bin/mini-go-dev ./cmd/mini-go-dev
	@cargo build $(cargo_flags) -p mini-go-rpc-peer-rust
	@metadata="$$(cargo metadata $(cargo_flags) --no-deps --format-version=1)" && \
		target="$$(printf '%s' "$$metadata" | node -pe 'JSON.parse(require("node:fs").readFileSync(0, "utf8")).target_directory')" && \
		MINIGO_RPC_GO_PEER="$(CURDIR)/bin/mini-go-rpc-peer-go" MINIGO_RPC_RUST_PEER="$$target/debug/mini-go-rpc-peer-rust" \
		go test ./integrations -run '^TestRPC.*Conformance$$' -count=1 -timeout=3m
	@MINIGO_HOST_BROKER="$(CURDIR)/bin/mini-go-dev" cargo test $(cargo_flags) --profile $(rust_test_profile) -p mini-go --features host-conformance --test stdlib -- --nocapture

# 依赖安装显式执行，npm build 不回调根生成流程。
.PHONY: deps _typescript-compiler-assets build-typescript lint-typescript test-typescript bench-typescript pack-typescript test-vscode
deps: ## 按 lockfile 安装 SDK 开发依赖
	@$(npm) ci

_typescript-compiler-assets:
	@mkdir -p "$(typescript_compiler_dir)"
	@GOOS=js GOARCH=wasm go build -trimpath -o "$(typescript_compiler_wasm)" ./cmd/mini-go-compiler-wasm
	@go_root="$$(go env GOROOT)"; \
		wasm_exec="$$go_root/lib/wasm/wasm_exec.js"; \
		test -f "$$wasm_exec" || { echo "Go wasm_exec.js was not found below $$go_root" >&2; exit 1; }; \
		install -m 0644 "$$wasm_exec" "$(typescript_compiler_dir)/wasm_exec.js"

build-typescript: _typescript-compiler-assets ## 构建 TypeScript runtime、Worker 与 Go compiler WASM 分发
	@$(npm) run build

lint-typescript: build-typescript ## SDK strict 类型、生成绑定与格式检查
	@$(npm) run lint

test-typescript: _check-execution-image _check-compiler-image lint-typescript _rpc-go-peer ## Node、浏览器、RPC、Go compiler、debug 与安装包测试
	@MINIGO_RPC_GO_PEER="$(CURDIR)/bin/mini-go-rpc-peer-go" $(npm) test

bench-typescript: _check-execution-image build-typescript ## TypeScript VM 与 Worker SDK 诊断基准（不设性能门槛）
	@$(npm) run benchmark

test-vscode: ## 验证并打包 VS Code 扩展
	@npm --prefix vscode-ext ci
	@npm --prefix vscode-ext test
	@npm --prefix vscode-ext run package

pack-typescript: build-typescript ## 将当前构建打包到 TYPESCRIPT_PACK_OUTPUT
	@mkdir -p "$(TYPESCRIPT_PACK_OUTPUT)"
	@cd $(typescript_dir) && npm pack --ignore-scripts --pack-destination "$(TYPESCRIPT_PACK_OUTPUT)"

.PHONY: test-scripts release-package release-verify clean cache-clean
test-scripts: ## 验证构建编排、CI 与发布脚本
	@node --test scripts/*.test.mjs .github/scripts/*.test.mjs

release-package: ## 在 RELEASE_OUTPUT 生成 crate 与 npm tarball
	@bash scripts/release-package.sh --output "$(RELEASE_OUTPUT)" $(RELEASE_FLAGS)

release-verify: ## 解包并验证 RELEASE_OUTPUT 中的分发产物
	@bash scripts/release-verify.sh --output "$(RELEASE_OUTPUT)"

clean: ## 清理项目构建与覆盖率产物
	@$(RM) -r bin build "$(COVERAGE_PROFILE)" "$(COVERAGE_HTML)"

cache-clean: ## 显式清理 Mini-Go 编译缓存
	@$(minigo) cache clean
