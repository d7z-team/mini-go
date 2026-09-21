#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

if awk '
/^\[/ {
	dependencies = $0 == "[dependencies]" || $0 ~ /^\[target\..*\.dependencies\]$/
}
dependencies && /^mini-go-tooling[[:space:]]*=/ {
	found = 1
}
END {
	exit !found
}
' playground/runtime-rust/Cargo.toml; then
	printf 'Rust runtime imports tooling\n' >&2
	exit 1
fi

if rg -n '(crate|super)::(rpc|stdlib_host)\b|\b(tokio|futures_util|tokio_rustls|tokio_tungstenite)::' playground/runtime-rust/src --glob '*.rs' --glob '!rpc/**' --glob '!**/rpc/**' --glob '!stdlib_host/**' --glob '!**/stdlib_host/**'; then
	printf 'the Rust VM core imports RPC or an asynchronous transport dependency\n' >&2
	exit 1
fi

if [[ -n $(find compiler runtime stdlib rpc tooling cmd -type d -name internal -print -quit) ]]; then
	printf 'source tree contains an internal directory\n' >&2
	exit 1
fi

if ! package_data=$(go list -f '{{.ImportPath}}|{{.Name}}|{{join .Imports " "}}' \
	./compiler/... ./runtime/... ./stdlib/... ./rpc/... ./tooling/... ./cmd/...); then
	printf 'go list failed; dependency boundary checks were not completed\n' >&2
	exit 1
fi

awk -F '[|]' -v module_prefix='github.com/d7z-team/mini-go/' '
function has_import(name, subpackages,    expected, i, count, imports) {
	expected = module_prefix name
	count = split($3, imports, " ")
	for (i = 1; i <= count; i++) {
		if (imports[i] == expected || (subpackages && index(imports[i], expected "/") == 1)) {
			return 1
		}
	}
	return 0
}
function is_package(name, subpackages,    expected) {
	expected = module_prefix name
	return $1 == expected || (subpackages && index($1, expected "/") == 1)
}
function violation(message) {
	print message ": " $1 > "/dev/stderr"
	failed = 1
}
{
	if (is_package("runtime", 1) && has_import("compiler", 0)) {
		violation("runtime imports the compiler facade")
	}
	if (is_package("compiler", 1) && has_import("runtime", 0)) {
		violation("compiler imports the runtime implementation")
	}
	if (is_package("compiler", 1) && has_import("tooling", 1)) {
		violation("compiler imports tooling")
	}
	if ((is_package("compiler", 1) || is_package("runtime", 1)) &&
		(has_import("rpc", 1) || has_import("tooling/mrpc", 1))) {
		violation("compiler or runtime imports RPC or MRPC tooling")
	}
	if ($1 == module_prefix "rpc" &&
		(has_import("compiler", 1) || has_import("runtime", 1) ||
		 has_import("stdlib", 1) || has_import("tooling", 1))) {
		violation("the RPC package imports compiler, VM, stdlib, or tooling packages")
	}
	if ($1 == module_prefix "stdlib" &&
		(has_import("compiler", 1) || has_import("runtime", 1) ||
		 has_import("rpc", 1) || has_import("tooling", 1))) {
		violation("the standard-library source package imports project implementation packages")
	}
	if (is_package("stdlib/host", 1) &&
		(has_import("compiler", 1) || has_import("runtime", 1) ||
		 has_import("tooling", 1) || has_import("cmd", 1))) {
		violation("a standard-library host package imports compiler, runtime, tooling, or command packages")
	}
	if ((is_package("compiler", 1) || is_package("runtime", 1) ||
		 is_package("stdlib", 1) || is_package("rpc", 1) || is_package("tooling", 1)) &&
		$2 == "main") {
		violation("a library-owned directory contains an executable package")
	}
	if (is_package("cmd", 1) && $2 != "main") {
		violation("the command tree contains a non-executable package")
	}
}
END {
	exit failed
}
' <<<"$package_data"
