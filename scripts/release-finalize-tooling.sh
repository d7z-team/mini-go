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
		echo "usage: release-finalize-tooling.sh [--output PATH]" >&2
		exit 2
		;;
	esac
done

manifest="$output/release.json"
test -f "$manifest"
version=$(node -e 'const value=require(process.argv[1]);process.stdout.write(value.version)' "$manifest")
rust="$output/source/playground/runtime-rust"
cargo_target=${CARGO_TARGET_DIR:-"$output/cargo-target"}

# Cargo can only produce the final publish archive after the exact mini-go
# dependency is visible in crates.io. The pre-publication archive deliberately
# omits Cargo.lock; this command replaces it with Cargo's final archive.
CARGO_TARGET_DIR="$cargo_target" cargo package \
	--manifest-path "$rust/Cargo.toml" --locked -p mini-go-tooling --no-verify
cp "$cargo_target/package/mini-go-tooling-$version.crate" "$output/"
tar -tf "$output/mini-go-tooling-$version.crate" >"$output/mini-go-tooling.files"

node - "$manifest" <<'NODE'
const { readFileSync, writeFileSync } = require("node:fs");
const filename = process.argv[2];
const value = JSON.parse(readFileSync(filename, "utf8"));
value.toolingFinalized = true;
writeFileSync(filename, `${JSON.stringify(value, null, 2)}\n`);
NODE

(cd "$output" && sha256sum \
	"mini-go-$version.crate" \
	"mini-go-tooling-$version.crate" \
	"d7z-team-mini-go-$version.tgz") >"$output/SHA256SUMS"
echo "Finalized mini-go-tooling $version after mini-go registry publication"
