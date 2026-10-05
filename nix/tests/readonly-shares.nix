# Read-only shares must hold against guest root, not only against the mount
# option a guest can remount away. Under QEMU, virtiofsd's --readonly is what
# refuses the write; vfkit's virtio-fs has no host-side equivalent, so there
# only the unprivileged-write half applies.
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
      if guest a 'mount -o remount,rw /nix/.ro-store && touch /nix/.ro-store/breach'; then
        fail "guest root wrote into the host store after a remount"
      fi
      test ! -e /nix/store/breach
      if guest a 'mount -o remount,rw /root/ro-src && touch /root/ro-src/breach'; then
        fail "guest root wrote through a read-only credential after a remount"
      fi
    fi
    test ! -e "$SPROUT_TEST_DIR/ro-src/breach"
  '';
}
