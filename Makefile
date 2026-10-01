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
WASM_PACK_OUTPUT ?= $(CURDIR)/build/npm
RELEASE_OUTPUT ?= $(CURDIR)/build/release
RELEASE_FLAGS ?=

golangci_lint := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2
minigo := go run ./cmd/mini-go
minigo_dev := go run ./cmd/mini-go-dev
minigo_sources = $(shell find stdlib/src stdlib/host -type f \( -name '*.mgo' -o -name '*.mrpc' \) -print | sort) rpc/gateway/control.mrpc
rust_dir := playground/runtime-rust
cargo_flags := --locked --manifest-path $(rust_dir)/Cargo.toml
wasm_dir := $(rust_dir)/runtime-wasm
npm := npm --prefix $(wasm_dir)
wasm_fixtures := $(CURDIR)/build/wasm-fixtures
runtime_images := testdata/runtime/execution.json.gz testdata/runtime/stdlib.json.gz
compiler_image := $(rust_dir)/assets/compiler.json.gz
wasm_cargo_flags := --target wasm32-unknown-unknown
ifeq ($(MINIGO_BUILD_STD),1)
wasm_cargo_flags += -Z build-std=std,panic_abort
wasm_env := RUSTC_BOOTSTRAP=1
endif

.PHONY: help check generate doc artifacts compiler-image _check-identity _check-runtime-images _check-artifacts
help: ## 显示常用目标与说明
	@awk 'BEGIN { print "用法：make <目标> [变量=值]\n" } /^[a-zA-Z][a-zA-Z0-9_-]*:.*## / { split($$0, parts, ":"); sub(/^.*## /, ""); printf "  %-24s %s\n", parts[1], $$0 }' $(MAKEFILE_LIST)

check: lint test build lint-rust test-rust test-interop test-wasm test-scripts ## 完整验证；先准备工具、deps 和生成产物

# generate 更新全部事实；artifacts 只在身份匹配时补齐未随 Git 分发的镜像。
generate: ## 更新全部绑定、契约、镜像与 manifest
	@go generate .

doc: ## 从源码注释生成 API 文档
	@$(minigo) doc -out docs/reference std

artifacts: _check-identity $(runtime_images) $(compiler_image) ## 显式补齐缺失的 runtime、stdlib 和 compiler 镜像

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
	@for image in $(runtime_images); do test -f "$$image" || { echo "Missing $$image; run make artifacts (or make generate after source changes)" >&2; exit 1; }; done

_check-artifacts: _check-runtime-images
	@test -f "$(compiler_image)" || { echo "Missing $(compiler_image); run make compiler-image" >&2; exit 1; }

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

test-rust: _check-artifacts ## release 原生 VM、compiler、RPC、Host 与标准库测试
	@cargo test $(cargo_flags) --release -p mini-go --features language-server,rpc-gateway,stdlib-host --no-fail-fast $(RUST_TEST_FLAGS)

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
	@MINIGO_HOST_BROKER="$(CURDIR)/bin/mini-go-dev" cargo test $(cargo_flags) --release -p mini-go --features host-conformance --test stdlib -- --nocapture

# 依赖安装显式执行，npm build 不回调根生成流程。
.PHONY: deps build-wasm lint-wasm test-wasm pack-wasm
deps: ## 按 lockfile 安装 SDK 开发依赖
	@$(npm) ci

build-wasm: _check-artifacts ## 构建 TypeScript、Worker、WASM 与 compiler 分发
	@$(npm) run build

lint-wasm: build-wasm ## SDK 类型/格式与 wasm32 Clippy
	@$(npm) run lint
	@$(wasm_env) cargo clippy $(cargo_flags) $(wasm_cargo_flags) -p mini-go -p mini-go-wasm --all-features -- -D warnings

test-wasm: lint-wasm _rpc-go-peer ## WASM driver、Node、浏览器、RPC、语言工具与安装包测试
	@MINIGO_WASM_FIXTURES="$(wasm_fixtures)" cargo test $(cargo_flags) --test wasm_driver
	@MINIGO_WASM_FIXTURES="$(wasm_fixtures)" MINIGO_RPC_GO_PEER="$(CURDIR)/bin/mini-go-rpc-peer-go" $(npm) test

pack-wasm: build-wasm ## 将当前构建打包到 WASM_PACK_OUTPUT
	@mkdir -p "$(WASM_PACK_OUTPUT)"
	@cd $(wasm_dir) && npm pack --ignore-scripts --pack-destination "$(WASM_PACK_OUTPUT)"

.PHONY: test-scripts release-package release-verify clean cache-clean
test-scripts: ## 验证构建编排、CI 与发布脚本
	@node --test scripts/*.test.mjs .github/scripts/*.test.mjs

release-package: test-scripts ## 在 RELEASE_OUTPUT 生成 crate 与 npm tarball
	@bash scripts/release-package.sh --output "$(RELEASE_OUTPUT)" $(RELEASE_FLAGS)

release-verify: ## 解包并验证 RELEASE_OUTPUT 中的分发产物
	@bash scripts/release-verify.sh --output "$(RELEASE_OUTPUT)"

clean: ## 清理项目构建与覆盖率产物
	@$(RM) -r bin build "$(COVERAGE_PROFILE)" "$(COVERAGE_HTML)"

cache-clean: ## 显式清理 Mini-Go 编译缓存
	@$(minigo) cache clean
