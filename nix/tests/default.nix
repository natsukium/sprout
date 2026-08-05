# sproutTests: boot tests in the spirit of nixosTests, but driven by sprout's
# own stack (vfkit via microvm.nix) instead of the QEMU test driver.
#
# Unlike nixosTests these cannot run *inside* a derivation: vfkit needs
# Virtualization.framework entitlements and system services the Nix build
# sandbox denies. So the split is: `nix build` produces the test package
# (bundle + driver script, fully reproducible), and execution happens outside
# the sandbox — `nix run .#sproutTests.<name>` on a Mac, or `.all` for the
# whole suite.
#
# The driver is the real `sprout` binary rather than a bespoke harness: what
# these tests exist to cover is the Nix -> manifest -> Go placeholder
# substitution -> virtiofs -> guest contract, and only the shipped CLI
# exercises all of it.
#
# A test file is `{ lib, pkgs, ... }: { vm; testScript; }`: `vm` is a VM
# definition module, `testScript` is bash run with `up <instance>` and
# `guest <instance> '<command>'` in scope. `guest` runs the command under a
# login shell so guestEnv (environment.variables) is populated, matching
# what a user sees in `sprout shell`. $SPROUT_TEST_DIR points at the
# driver's scratch tree for host-side fixtures (a `mount` source, etc.).
{
  localInputs,
  lib,
  pkgs,
  sprout,
}:
let
  sproutLib = import ../lib.nix { inherit localInputs lib; };

  mkSproutTest =
    name: testFn:
    let
      test = testFn { inherit lib pkgs; };
      bundle = sproutLib.mkVM {
        inherit pkgs;
        name = "test-${name}";
        imports = [
          test.vm
          # The workspace mount is off because the driver runs from a scratch
          # directory that is not a repository.
          {
            vcpu = lib.mkDefault 1;
            mem = lib.mkDefault 1024;
            workspace = lib.mkDefault false;
          }
        ];
      };
    in
    pkgs.writeShellApplication {
      name = "sprout-test-${name}";
      runtimeInputs = [ sprout ] ++ sproutLib.hostTools pkgs;
      # Single-quoted $VARs in test scripts are deliberate: they must reach
      # the guest shell unexpanded, which is exactly what SC2016 flags.
      excludeShellChecks = [ "SC2016" ];
      text = ''
        echo "=== sprout test: ${name}"
        SPROUT_TEST_DIR=$(mktemp -d)
        export SPROUT_TEST_DIR
        export XDG_STATE_HOME="$SPROUT_TEST_DIR/state"
        export XDG_CACHE_HOME="$SPROUT_TEST_DIR/cache"
        export XDG_CONFIG_HOME="$SPROUT_TEST_DIR/config"
        cd "$SPROUT_TEST_DIR" || exit 1

        started=()
        cleanup() {
          for i in "''${started[@]}"; do
            sprout delete --force --instance "$i" >/dev/null 2>&1 || true
          done
          rm -rf "$SPROUT_TEST_DIR"
        }
        trap cleanup EXIT

        up() {
          started+=("$1")
          sprout up --bundle ${bundle} --instance "$1"
        }
        guest() {
          local i="$1"
          shift
          # Pass the script as one -c operand; sprout quotes each argv element
          # for the SSH remote shell, so pre-escaping here would turn the whole
          # script into a literal command name.
          sprout exec --instance "$i" -- sh -lc "$*"
        }

        ${test.testScript}
        echo "=== ok: ${name}"
      '';
    };

  testFiles = lib.filterAttrs (
    fname: type: type == "regular" && fname != "default.nix" && lib.hasSuffix ".nix" fname
  ) (builtins.readDir ./.);

  tests = lib.mapAttrs' (
    fname: _:
    let
      name = lib.removeSuffix ".nix" fname;
    in
    lib.nameValuePair name (mkSproutTest name (import (./. + "/${fname}")))
  ) testFiles;
in
tests
// {
  all = pkgs.writeShellApplication {
    name = "sprout-tests-all";
    text = ''
      failed=()
      ${lib.concatMapStringsSep "\n" (t: ''
        ${lib.getExe t} || failed+=("${t.name}")
      '') (lib.attrValues tests)}
      if [ "''${#failed[@]}" -gt 0 ]; then
        echo "=== failed: ''${failed[*]}"
        exit 1
      fi
      echo "=== all sprout tests passed"
    '';
  };
}
