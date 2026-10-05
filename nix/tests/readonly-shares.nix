# Read-only shares must hold against guest root, not only against the mount
# option a guest can remount away. Under QEMU, virtiofsd's --readonly is what
# refuses the write; vfkit's virtio-fs has no host-side equivalent, so there
# only the mount-option half applies.
{ ... }:
{
  vm.credentials.ro-fixture = {
    enable = true;
    strategy = "mount";
    source = "$SPROUT_TEST_DIR/ro-src";
    target = "/root/ro-src";
  };

  testScript = ''
    mkdir -p "$SPROUT_TEST_DIR/ro-src"
    echo hello >"$SPROUT_TEST_DIR/ro-src/file"
    up a

    if guest a 'touch /nix/store/breach'; then
      fail "guest wrote into the host store"
    fi
    if guest a 'touch /root/ro-src/breach'; then
      fail "guest wrote through a read-only credential mount"
    fi

    if [ "$(uname -s)" = Linux ]; then
      # The host user owns the fixture, so once the guest-side option is gone
      # only virtiofsd stands between guest root and the write.
      guest a 'mount -o remount,rw /root/ro-src' || fail "guest root could not remount the credential share"
      guest a 'grep " /root/ro-src " /proc/mounts' | grep -q '[[:space:]]rw[,[:space:]]' || fail "the remount left the share read-only in the guest"
      if guest a 'touch /root/ro-src/breach'; then
        fail "guest root wrote through a read-only credential after a remount"
      fi
    fi
    test ! -e "$SPROUT_TEST_DIR/ro-src/breach"
  '';
}
