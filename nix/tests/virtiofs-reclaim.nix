{ pkgs, ... }:
{
  vm.modules = [
    (
      { pkgs, ... }:
      {
        networking.firewall.enable = true;
        environment.etc."reclaim-fixture".source = pkgs.runCommand "reclaim-fixture" { } ''
          mkdir "$out"
          for ((i = 0; i < 4096; i++)); do
            echo "$i" > "$out/$i"
          done
        '';
      }
    )
  ];

  testScript = ''
    up reclaim
    manifest="$(instance_dir reclaim)/bundle/manifest.json"
    port=$(jq -er '.guest.reclaimPort' "$manifest")
    ip=$(jq -er '.guest.ip' "$manifest")

    guest reclaim "if timeout 3 bash -c 'exec 3<>/dev/tcp/127.0.0.1/$port'; then exit 1; fi" \
      || fail "the reclaim socket accepted a connection from loopback"

    # Stat the fixture on virtiofs so the test has reclaimable inodes even
    # when the host's store contains only the guest closure.
    cache_fixture() {
      guest reclaim '
        fixture=$(readlink -f /etc/reclaim-fixture) &&
        stat "/nix/.ro-store/$(basename "$fixture")"/* >/dev/null
      '
    }

    request_reclaim() {
      local deadline=30
      # Keep stdin open past the deadline so only guest EOF can end the dial.
      # The sentinel preserves the reply's trailing newline.
      reply=$(${pkgs.coreutils}/bin/timeout "''${deadline}s" sprout dial-stdio -i reclaim "$ip:$port" < <(sleep "$((deadline + 5))") && printf '.') \
        || fail "reclaim dial failed or stayed open for at least $deadline s"
    }

    cache_fixture
    request_reclaim
    IFS=' ' read -r before after <<< "$reply"
    [[ "$before" =~ ^[0-9]+$ && "$after" =~ ^[0-9]+$ && "$reply" == "$before $after"$'\n.' ]] \
      || fail "invalid reclaim reply: $reply"
    echo "fuse_inode: $before before, $after after"
    (( before >= 3000 && after < before / 2 )) || fail "expected reclaim to free at least half the cached inodes"

    cache_fixture
    request_reclaim
    [[ "$reply" == $'skipped\n.' ]] || fail "expected a skipped reclaim, got: $reply"
  '';
}
