#!/usr/bin/env bash
set -euo pipefail

repository=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
output="$repository/build/release"
allow_dirty=false
while (($#)); do
	case "$1" in
	--allow-dirty)
		allow_dirty=true
		shift
		;;
	--output)
		output=$(realpath -m "$2")
		shift 2
		;;
	*)
		echo "usage: release-package.sh [--allow-dirty] [--output PATH]" >&2
		exit 2
		;;
	esac
done

release_json=$(node "$repository/scripts/release-version.mjs" --json --repository "$repository")
version=$(node -e 'const value=JSON.parse(process.argv[1]);process.stdout.write(value.version)' "$release_json")
commit=$(node -e 'const value=JSON.parse(process.argv[1]);process.stdout.write(value.commit)' "$release_json")
commit_count=$(node -e 'const value=JSON.parse(process.argv[1]);process.stdout.write(String(value.commitCount))' "$release_json")
short_commit=$(node -e 'const value=JSON.parse(process.argv[1]);process.stdout.write(value.shortCommit)' "$release_json")

if [[ -n $(git -C "$repository" status --porcelain=v1 --untracked-files=all) ]]; then
	if [[ "$allow_dirty" != true ]]; then
		echo "release packaging requires a clean Git worktree" >&2
		exit 1
	fi
	version="$version.dirty"
fi

rm -rf "$output"
mkdir -p "$output/source"
scratch=$(mktemp -d "${TMPDIR:-/tmp}/mini-go-release.XXXXXX")
trap 'rm -rf "$scratch"' EXIT
export GOCACHE=${GOCACHE:-"$scratch/go-build"}
export MINIGO_CACHE=${MINIGO_CACHE:-"$scratch/mini-go"}
export NPM_CONFIG_CACHE=${NPM_CONFIG_CACHE:-"$scratch/npm"}
if [[ "$allow_dirty" == true ]]; then
	git -C "$repository" ls-files --cached --others --exclude-standard -z |
		while IFS= read -r -d '' file; do
			if [[ -e "$repository/$file" || -L "$repository/$file" ]]; then printf '%s\0' "$file"; fi
		done |
		tar -C "$repository" --null --files-from=- -cf - |
		tar -C "$output/source" -xf -
else
	git -C "$repository" archive --format=tar HEAD | tar -C "$output/source" -xf -
fi

source_root="$output/source"
rust="$source_root/playground/runtime-rust"
typescript="$source_root/playground/runtime-typescript"
cargo_target=${CARGO_TARGET_DIR:-"$output/cargo-target"}

node "$source_root/scripts/release-version.mjs" --set "$version" --repository "$source_root"
make -C "$source_root" compiler-image

compiler="$rust/assets/compiler.json.gz"
runtime_compiler_sha256=$(sha256sum "$compiler" | cut -d ' ' -f 1)
export CARGO_TARGET_DIR="$cargo_target"
cargo fetch --manifest-path "$rust/Cargo.toml" --locked
cargo package --manifest-path "$rust/Cargo.toml" --locked --offline -p mini-go
cp "$cargo_target/package/mini-go-$version.crate" "$output/"

npm --prefix "$typescript" ci
make -C "$source_root" build-typescript
if [[ $(sha256sum "$compiler" | cut -d ' ' -f 1) != "$runtime_compiler_sha256" ]]; then
	echo "TypeScript build replaced the Rust runtime compiler image" >&2
	exit 1
fi
typescript_compiler_sha256=$(sha256sum "$typescript/dist/compiler/mini-go-compiler.wasm" | cut -d ' ' -f 1)
(cd "$typescript" && npm pack --ignore-scripts --json --pack-destination "$output") >"$output/npm-pack.json"
npm_tarball="$output/d7z-team-mini-go-$version.tgz"
test -f "$npm_tarball"
npm_integrity=$(node -e 'const {createHash}=require("node:crypto");const {readFileSync}=require("node:fs");process.stdout.write(`sha512-${createHash("sha512").update(readFileSync(process.argv[1])).digest("base64")}`)' "$npm_tarball")

tar -tf "$output/mini-go-$version.crate" >"$output/mini-go.files"
tar -tf "$npm_tarball" >"$output/npm.files"

node - "$output/release.json" "$version" "$commit" "$commit_count" "$short_commit" "$runtime_compiler_sha256" "$typescript_compiler_sha256" "$npm_integrity" <<'NODE'
const { writeFileSync } = require("node:fs");
const [filename, version, commit, count, shortCommit, runtimeCompilerSHA256, typescriptCompilerSHA256, npmIntegrity] = process.argv.slice(2);
writeFileSync(filename, `${JSON.stringify({
  version,
  commit,
  commitCount: Number(count),
  shortCommit,
  runtimeCompilerSHA256,
  typescriptCompilerSHA256,
  npmIntegrity,
  crates: [`mini-go-${version}.crate`],
  npm: `d7z-team-mini-go-${version}.tgz`,
}, null, 2)}\n`);
NODE

(cd "$output" && sha256sum "mini-go-$version.crate" "$(basename "$npm_tarball")") >"$output/SHA256SUMS"
echo "Packaged Mini-Go $version in $output"
