#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

# The examples reach sprout through a relative `path:../..` input rather than a
# copy that could drift from what readers run, so they need Nix 2.26 or newer,
# where such an input resolves under pure evaluation.
for example in minimal podman k3s; do
  echo "=== examples/$example ==="
  (
    cd "$root/examples/$example"
    nix flake check --no-build --print-build-logs
    # `nix flake check` skips sproutConfigurations as an unknown flake output,
    # leaving the manifest and runner plumbing in nix/bundle.nix unevaluated.
    # Forcing the drvPath evaluates the whole guest system without realising
    # its aarch64-linux closure, so this stays a host-side check.
    nix eval --raw --apply \
      'cfgs: builtins.concatStringsSep "\n" (map (c: c.drvPath) (builtins.attrValues cfgs))' \
      '.#sproutConfigurations'
    echo
  )
done
