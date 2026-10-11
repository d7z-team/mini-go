#!/usr/bin/env bash
set -euo pipefail

repository=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
output="$repository/build/release"
while (($#)); do
	case "$1" in
	--output)
		output=$(realpath -m "$2")
		shift 2
		;;
	*)
		echo "usage: release-verify.sh [--output PATH]" >&2
		exit 2
		;;
	esac
done

manifest="$output/release.json"
test -f "$manifest"
version=$(node -e 'const value=require(process.argv[1]);process.stdout.write(value.version)' "$manifest")
expected_runtime_compiler=$(node -e 'const value=require(process.argv[1]);process.stdout.write(value.runtimeCompilerSHA256)' "$manifest")
expected_typescript_compiler=$(node -e 'const value=require(process.argv[1]);process.stdout.write(value.typescriptCompilerSHA256)' "$manifest")
expected_npm_integrity=$(node -e 'const value=require(process.argv[1]);process.stdout.write(value.npmIntegrity)' "$manifest")
runtime_crate="$output/mini-go-$version.crate"
npm_tarball="$output/d7z-team-mini-go-$version.tgz"
source_root="$output/source"
for file in "$runtime_crate" "$npm_tarball"; do test -f "$file"; done
(cd "$output" && sha256sum --check SHA256SUMS)
actual_npm_integrity=$(node -e 'const {createHash}=require("node:crypto");const {readFileSync}=require("node:fs");process.stdout.write(`sha512-${createHash("sha512").update(readFileSync(process.argv[1])).digest("base64")}`)' "$npm_tarball")
if [[ "$actual_npm_integrity" != "$expected_npm_integrity" ]]; then
	echo "npm tarball integrity does not match the release manifest" >&2
	exit 1
fi

if grep -Eq '^[^/]+/(tests|target|cmd)/|/go\.mod$|/\.gitignore$' "$output/mini-go.files"; then
	echo "mini-go crate contains repository-only files" >&2
	exit 1
fi
if [[ $(grep -c '/assets/compiler\.json\.gz$' "$output/mini-go.files") -ne 1 ]]; then
	echo "mini-go crate must contain exactly one compiler image" >&2
	exit 1
fi
if ! grep -q '/LICENSE-Go$' "$output/mini-go.files"; then
	echo "mini-go crate is missing the Go license" >&2
	exit 1
fi
if grep -Eq '/(tests|target|node_modules|src)/' "$output/npm.files"; then
	echo "npm tarball contains development files" >&2
	exit 1
fi
if [[ $(grep -c '^package/dist/compiler/mini-go-compiler\.wasm$' "$output/npm.files") -ne 1 ]] ||
	[[ $(grep -c '^package/dist/compiler/wasm_exec\.js$' "$output/npm.files") -ne 1 ]]; then
	echo "npm tarball is missing its Go compiler assets" >&2
	exit 1
fi
if ! grep -q '^package/LICENSE-Go$' "$output/npm.files"; then
	echo "npm tarball is missing the Go license" >&2
	exit 1
fi

temporary=$(mktemp -d "${TMPDIR:-/tmp}/mini-go-release-verify.XXXXXX")
trap 'rm -rf "$temporary"' EXIT
export NPM_CONFIG_CACHE=${NPM_CONFIG_CACHE:-"$temporary/npm-cache"}
tar -xf "$runtime_crate" -C "$temporary"
mkdir "$temporary/npm"
tar -xf "$npm_tarball" -C "$temporary/npm"
node -e 'const value=require(process.argv[1]);if(value.version!==process.argv[2])process.exit(1)' \
	"$temporary/npm/package/package.json" "$version"
runtime="$temporary/mini-go-$version"
runtime_compiler=$(sha256sum "$runtime/assets/compiler.json.gz" | cut -d ' ' -f 1)
typescript_compiler=$(sha256sum "$temporary/npm/package/dist/compiler/mini-go-compiler.wasm" | cut -d ' ' -f 1)
if [[ "$runtime_compiler" != "$expected_runtime_compiler" || "$typescript_compiler" != "$expected_typescript_compiler" ]]; then
	echo "distributed compiler artifacts do not match the release manifest" >&2
	exit 1
fi

consumer="$temporary/consumer"
mkdir "$consumer"
cat >"$consumer/Cargo.toml" <<EOF
[package]
name = "mini-go-release-consumer"
version = "0.0.0"
edition = "2024"

[dependencies]
mini-go = { path = "$runtime", features = ["rpc-gateway", "stdlib-host", "language-server"] }
serde_json = "1"
tokio = { version = "1", features = ["macros", "rt"] }
EOF
mkdir "$consumer/src"
cat >"$consumer/src/main.rs" <<'EOF'
use mini_go::ffi::Cancellation;
use mini_go::{LanguageService, CompilerSession};
use serde_json::json;

#[tokio::main(flavor = "current_thread")]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let cancel = Cancellation::default();
    let mut language = LanguageService {
        session: CompilerSession::bundled().await?,
    };
    language
        .open(
            json!({
                "Root": "probe",
                "Packages": [{
                    "Namespace": "module:probe",
                    "PackagePath": "",
                    "ModulePath": "probe",
                    "Files": [{"Path": "main.mgo", "Text": "package main\nfunc main() {}\n"}]
                }]
            }),
            &cancel,
        )
        .await?;
    let analysis = language.analyze(&cancel).await?;
    if analysis["Diagnostics"].as_array().is_some_and(|items| !items.is_empty()) {
        return Err(format!("release compiler diagnostics: {analysis}").into());
    }
    language.close().await?;
    Ok(())
}
EOF
consumer_target=${CARGO_TARGET_DIR:-"$output/cargo-target"}
CARGO_TARGET_DIR="$consumer_target" cargo generate-lockfile --manifest-path "$consumer/Cargo.toml"
CARGO_TARGET_DIR="$consumer_target" cargo run --release --manifest-path "$consumer/Cargo.toml" --locked

MINIGO_PACKAGE_TARBALL="$npm_tarball" \
	node --test "$source_root/playground/runtime-typescript/tests/package.test.js"
npm publish "$npm_tarball" --dry-run --tag git --access public >/dev/null

echo "Verified Mini-Go $version release artifacts"
