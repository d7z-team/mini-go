#!/usr/bin/env bash
set -euo pipefail

list=false
if [[ ${1:-} == --list ]]; then
	list=true
	shift
fi
if (($# == 0)); then set -- ./...; fi

packages=$(go list "$@")
targets=()
while IFS= read -r package; do
	[[ -n $package ]] || continue
	listed=$(go test "$package" -run '^$' -list "${FUZZ_PATTERN:-^Fuzz}")
	while IFS= read -r name; do
		if [[ $name == Fuzz* && $name != *[[:space:]]* ]]; then
			targets+=("$package $name")
		fi
	done <<<"$listed"
done <<<"$packages"

if ((${#targets[@]} == 0)); then
	echo "No fuzz targets match the selected packages and FUZZ_PATTERN" >&2
	exit 1
fi
for target in "${targets[@]}"; do
	read -r package name <<<"$target"
	printf '%s %s\n' "$package" "$name"
	if [[ $list == false ]]; then
		go test "$package" -run '^$' -fuzz "^${name}$" \
			-fuzztime="${FUZZTIME:-10s}" -parallel="${FUZZ_PARALLEL:-1}"
	fi
done
