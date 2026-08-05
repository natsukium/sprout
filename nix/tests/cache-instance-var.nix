# An instance-scoped cache is backed by the guest's own /var volume, not by a
# host share: it survives a stop, stays invisible to sibling instances, and
# leaves nothing under the host cache tree or the instance state directory.
{ ... }:
{
  vm.caches.scratch = {
    enable = true;
    guestPath = "/root/.cache/scratch";
    scope = "instance";
    guestEnv.SCRATCH_DIR = "/root/.cache/scratch";
  };
  testScript = ''
    up a
    up b

    guest a 'test "$SCRATCH_DIR" = /root/.cache/scratch'
    # The bind must be in place before the first guest command, or a tool
    # would write to the root tmpfs underneath it.
    guest a 'mountpoint -q /root/.cache/scratch'
    guest a 'touch /root/.cache/scratch/marker'
    guest b 'test ! -f /root/.cache/scratch/marker'

    # /var is the only volume that outlives a stop.
    sprout stop --instance a
    up a
    guest a 'test -f /root/.cache/scratch/marker'

    # No host-side footprint: the cache never becomes a virtiofs share, so
    # neither the shared tree nor the instance state directory gains an entry.
    test ! -e "$XDG_CACHE_HOME/sprout"
    test -z "$(find "$XDG_STATE_HOME/sprout/instances" -type d -name caches)"
  '';
}
