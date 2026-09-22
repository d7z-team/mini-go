#!/usr/bin/env bash
set -euo pipefail

: "${GITHUB_REPOSITORY:?GitHub repository is required}"
: "${GITHUB_SHA:?Release commit is required}"

for workflow in go-test.yml runtime-rust.yml; do
	runs=$(gh api --method GET "repos/$GITHUB_REPOSITORY/actions/workflows/$workflow/runs" \
		-f head_sha="$GITHUB_SHA" -f branch=main -f event=push -f per_page=1)
	if ! jq -e --arg sha "$GITHUB_SHA" \
		'.workflow_runs[0] | .head_sha == $sha and .head_branch == "main" and .event == "push" and .status == "completed" and .conclusion == "success"' \
		<<<"$runs" >/dev/null; then
		echo "::error::$workflow must succeed for commit $GITHUB_SHA before publishing; finish or rerun its push CI first"
		exit 1
	fi
	echo "$workflow passed for $GITHUB_SHA"
done
