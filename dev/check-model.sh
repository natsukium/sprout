#!/usr/bin/env bash
# Checks spec/lifecycle.qnt, then replays traces sampled from it against the Go
# implementation.
set -euo pipefail

cd "$(dirname "$0")/.."
spec=spec/lifecycle.qnt
traces=$(mktemp -d)
trap 'rm -rf "$traces"' EXIT

quint typecheck "$spec"
quint run "$spec" --invariant safety --max-samples "${MODEL_SAMPLES:-20000}" --max-steps 30 \
	--witnesses staleUpRejected swappedDeleteRejected runningDeleted lingeringBlocksDelete rebooted pruned adoptedSkipped recreated --verbosity 1
quint run "$spec" --invariant safety --max-steps 30 --n-traces "${MODEL_TRACES:-1000}" \
	--out-itf "$traces/trace{seq}.itf.json" --verbosity 0
SPROUT_MODEL_TRACES=$traces go test -count=1 -run TestLifecycleConformsToModel ./cmd/sprout
