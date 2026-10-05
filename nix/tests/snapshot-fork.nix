# /var round-trips through a snapshot and a fork: restore rolls a later
# write back, and a fork boots with the source's volume and outlives the
# source's deletion.
{ ... }:
{
  vm = { };

  testScript = ''
    marker() {
      guest "$1" 'cat /var/lib/sprout-marker'
    }

    up a
    guest a 'echo one >/var/lib/sprout-marker && sync'
    sprout stop --instance a
    sprout snapshot create --instance a one
    sprout snapshot list --instance a | grep -q '^one ' || fail "snapshot not listed"

    sprout start --instance a
    guest a 'echo two >/var/lib/sprout-marker && sync'
    sprout stop --instance a
    sprout snapshot restore --force --instance a one
    sprout start --instance a
    test "$(marker a)" = one || fail "restore did not roll /var back"

    sprout stop --instance a
    sprout fork --instance a b
    started+=(b)
    sprout start --instance b
    test "$(marker b)" = one || fail "fork did not carry /var"

    sprout delete --force --instance a
    test "$(marker b)" = one || fail "deleting the source disturbed the fork"
  '';
}
