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
expected_compiler=$(node -e 'const value=require(process.argv[1]);process.stdout.write(value.compilerSHA256)' "$manifest")
tooling_finalized=$(node -e 'const value=require(process.argv[1]);process.stdout.write(String(value.toolingFinalized))' "$manifest")
runtime_crate="$output/mini-go-$version.crate"
tooling_crate="$output/mini-go-tooling-$version.crate"
npm_tarball="$output/d7z-team-mini-go-$version.tgz"
source_root="$output/source"
for file in "$runtime_crate" "$tooling_crate" "$npm_tarball"; do test -f "$file"; done
(cd "$output" && sha256sum --check SHA256SUMS)

if grep -Eq '/(tests|target|cmd)/|/go\.mod$|/\.gitignore$|compiler\.json\.gz$' "$output/mini-go.files"; then
	echo "mini-go crate contains repository-only files" >&2
	exit 1
fi
if [[ $(grep -c '/assets/compiler\.json\.gz$' "$output/mini-go-tooling.files") -ne 1 ]]; then
	echo "mini-go-tooling crate must contain exactly one compiler image" >&2
	exit 1
fi
if ! grep -q '/LICENSE-Go$' "$output/mini-go-tooling.files"; then
	echo "mini-go-tooling crate is missing the Go license" >&2
	exit 1
fi
case "$tooling_finalized" in
true)
	grep -q '/Cargo.lock$' "$output/mini-go-tooling.files" || {
		echo "final mini-go-tooling crate is missing Cargo.lock" >&2
		exit 1
	}
	;;
false)
	if grep -q '/Cargo.lock$' "$output/mini-go-tooling.files"; then
		echo "pre-publication mini-go-tooling crate unexpectedly contains Cargo.lock" >&2
		exit 1
	fi
	;;
*)
	echo "release manifest has an invalid toolingFinalized value" >&2
	exit 1
	;;
esac
if grep -Eq '/(tests|target|node_modules|sdk)/' "$output/npm.files"; then
	echo "npm tarball contains development files" >&2
	exit 1
fi
if [[ $(grep -c '^package/dist/tools/compiler\.json\.gz$' "$output/npm.files") -ne 1 ]] ||
	[[ $(grep -c '^package/dist/wasm/mini_go_wasm_bg\.wasm$' "$output/npm.files") -ne 1 ]]; then
	echo "npm tarball is missing its compiler or WASM runtime" >&2
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
tar -xf "$tooling_crate" -C "$temporary"
mkdir "$temporary/npm"
tar -xf "$npm_tarball" -C "$temporary/npm"
node -e 'const value=require(process.argv[1]);if(value.version!==process.argv[2])process.exit(1)' \
	"$temporary/npm/package/package.json" "$version"
runtime="$temporary/mini-go-$version"
tooling="$temporary/mini-go-tooling-$version"
tooling_compiler=$(sha256sum "$tooling/assets/compiler.json.gz" | cut -d ' ' -f 1)
npm_compiler=$(sha256sum "$temporary/npm/package/dist/tools/compiler.json.gz" | cut -d ' ' -f 1)
if [[ "$tooling_compiler" != "$expected_compiler" || "$npm_compiler" != "$expected_compiler" ]]; then
	echo "distributed compiler images do not match the release image" >&2
	exit 1
fi

sed -i "/^\[dependencies\.mini-go\]$/a path = \"../mini-go-$version\"" "$tooling/Cargo.toml"
consumer="$temporary/consumer"
mkdir "$consumer"
cat >"$consumer/Cargo.toml" <<EOF
[package]
name = "mini-go-release-consumer"
version = "0.0.0"
edition = "2024"

[dependencies]
mini-go = { path = "$runtime", features = ["rpc-gateway", "stdlib-host"] }
mini-go-tooling = { path = "$tooling" }
serde_json = "1"
tokio = { version = "1", features = ["macros", "rt"] }
EOF
mkdir "$consumer/src"
cat >"$consumer/src/main.rs" <<'EOF'
use mini_go::ffi::Cancellation;
use mini_go_tooling::{language::LanguageService, session::CompilerSession};
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
CARGO_TARGET_DIR="$consumer_target" cargo run --manifest-path "$consumer/Cargo.toml" --locked

fixtures=${MINIGO_WASM_FIXTURES:-"$output/cargo-target/wasm-fixtures"}
if [[ ! -f "$fixtures/answer.json" ]]; then
	MINIGO_WASM_FIXTURES="$fixtures" CARGO_TARGET_DIR="$consumer_target" \
		cargo test --manifest-path "$source_root/playground/runtime-rust/Cargo.toml" --locked --test wasm_driver
fi
MINIGO_PACKAGE_TARBALL="$npm_tarball" MINIGO_WASM_FIXTURES="$fixtures" \
	node --test "$source_root/playground/runtime-rust/runtime-wasm/tests/package.test.js"
npm publish "$npm_tarball" --dry-run --tag git --access public >/dev/null

echo "Verified Mini-Go $version release artifacts"
