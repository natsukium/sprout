# materialize: the host exec runs at `up`, its stdout crosses the
# per-instance data share, and the guest copy carries the 0600 root perms
# credential-sensitive tools check. /bin/sh keeps the exec free of store
# paths that would differ between builder and runner host.
#
# mount: vfkit's virtio-fs device has no host-side ro flag, so the
# guest-side mount option is the only thing standing between a guest
# process and the host credential. The source rides $SPROUT_TEST_DIR ($VAR
# expansion is resolved by Go at up time), so the fixture can be created by
# the test script itself.
{ ... }:
{
  vm.credentials = {
    test-token = {
      enable = true;
      strategy = "materialize";
      exec = [
        "/bin/sh"
        "-c"
        "printf sprout-test-secret"
      ];
      guestPath = "/root/.config/test/token";
    };
    ro-fixture = {
      enable = true;
      strategy = "mount";
      source = "$SPROUT_TEST_DIR/ro-src";
      target = "/root/ro-src";
    };
  };
  testScript = ''
    mkdir -p "$SPROUT_TEST_DIR/ro-src"
    echo hello >"$SPROUT_TEST_DIR/ro-src/file"
    up a

    guest a 'test "$(cat /root/.config/test/token)" = sprout-test-secret'
    guest a 'test "$(stat -c %a /root/.config/test/token)" = 600'

    guest a 'test "$(cat /root/ro-src/file)" = hello'
    if guest a 'touch /root/ro-src/breach'; then
      echo "guest wrote through a read-only mount"
      exit 1
    fi
    test ! -e "$SPROUT_TEST_DIR/ro-src/breach"
  '';
}
