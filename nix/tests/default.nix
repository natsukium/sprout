# sproutTests: boot tests in the spirit of nixosTests, but driven by sprout's
# own stack (vfkit on macOS, QEMU with virtiofsd sidecars on Linux, both via
# microvm.nix) instead of the NixOS test driver.
#
# Unlike nixosTests these cannot run *inside* a derivation: vfkit needs
# Virtualization.framework entitlements and system services the Nix build
# sandbox denies, and the QEMU backend needs /dev/kvm and unprivileged user
# namespaces for virtiofsd. So the split is: `nix build` produces the test
# package (bundle + driver script, fully reproducible), and execution happens
# outside the sandbox — `nix run .#sproutTests.<name>` on the host, or `.all`
# for the whole suite.
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
# The other helpers (`fail`, `bg`, `eventually`, `inspect`, `vm_processes`,
# `assert_quiescent`, ...) are defined in the driver below.
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
      runtimeInputs = [
        sprout
        pkgs.curl
        pkgs.jq
        pkgs.lsof
      ]
      ++ sproutLib.hostTools pkgs;
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
        background=()
        cleanup() {
          for p in "''${background[@]}"; do
            kill "$p" 2>/dev/null || true
          done
          for i in "''${started[@]}"; do
            sprout delete --force --hard --instance "$i" >/dev/null 2>&1 || true
          done
          rm -rf "$SPROUT_TEST_DIR"
        }
        trap cleanup EXIT

        fail() {
          echo "FAIL: $*"
          exit 1
        }
        up() {
          started+=("$1")
          sprout up --bundle ${bundle} --instance "$1"
        }
        bg() {
          "$@" &
          background+=("$!")
        }
        # eventually SECONDS CMD...: retry CMD once a second until it succeeds.
        eventually() {
          local deadline=$(($(date +%s) + $1))
          shift
          until "$@"; do
            [ "$(date +%s)" -lt "$deadline" ] || return 1
            sleep 1
          done
        }
        inspect() {
          sprout inspect --instance "$1" | jq -r "$2"
        }
        instance_dir() {
          echo "$XDG_STATE_HOME/sprout/instances/$(inspect "$1" .id)"
        }
        # The running daemon and every process under it, captured while it
        # runs: once it is gone the tree can no longer be walked.
        vm_processes() {
          descendants "$(inspect "$1" .pid)"
        }
        descendants() {
          local p
          for p in "$@"; do
            echo "$p"
            # shellcheck disable=SC2046
            descendants $(pgrep -P "$p" || true)
          done
        }
        # A zombie still answers kill -0 but holds nothing.
        alive() {
          local p state
          for p in "$@"; do
            state=$(ps -o stat= -p "$p" 2>/dev/null || true)
            if [ -n "$state" ] && [ "''${state#Z}" = "$state" ]; then
              return 0
            fi
          done
          return 1
        }
        none_alive() {
          ! alive "$@"
        }
        # assert_quiescent DIR PID...: nothing the instance ran is left: no
        # process, no open handle on var.img, no socket file.
        assert_quiescent() {
          local dir=$1
          shift
          if ! eventually 30 none_alive "$@"; then
            ps -o pid=,command= -p "$(IFS=,; echo "$*")" || true
            fail "processes outlived the instance"
          fi
          if [ -e "$dir/var.img" ] && lsof -t -- "$dir/var.img" >/dev/null 2>&1; then
            lsof -- "$dir/var.img" || true
            fail "var.img is still open"
          fi
          if [ -d "$dir/sock" ] && [ -n "$(find "$dir/sock" -type s)" ]; then
            ls -l "$dir/sock"
            fail "stale sockets left behind"
          fi
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
