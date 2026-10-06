# A cache with an owner is writable by that non-root uid in both backings:
# the host share, where the guest otherwise sees the host user's 0700 tree as
# root's, and the instance volume, where the backing directory is root's.
# Nested writes are the point: an entry the tool creates must stay its own.
{ ... }:
{
  vm.caches = {
    hosted = {
      enable = true;
      guestPath = "/srv/hosted";
      owner = {
        uid = 65532;
        gid = 65532;
      };
    };
    local = {
      enable = true;
      guestPath = "/srv/local";
      scope = "instance";
      owner = {
        uid = 65532;
        gid = 65532;
      };
    };
    rooted = {
      enable = true;
      guestPath = "/srv/rooted";
    };
  };
  testScript = ''
    git init -q "$SPROUT_TEST_DIR/repo"
    cd "$SPROUT_TEST_DIR/repo"
    up a

    as_owner() {
      guest a "setpriv --reuid=65532 --regid=65532 --clear-groups sh -c '$1'"
    }

    # vfkit does not act on owner for a host share; the host-share checks
    # here and below are virtiofsd's mapping.
    caches=local
    if [ "$(uname -s)" = Linux ]; then
      caches="hosted local"
    fi
    for c in $caches; do
      test "$(guest a "stat -c %u:%g /srv/$c")" = 65532:65532 || fail "/srv/$c is not owned by its declared owner"
      as_owner "mkdir -p /srv/$c/sub && echo one >/srv/$c/sub/entry && echo two >>/srv/$c/sub/entry" ||
        fail "the owner could not write nested entries into /srv/$c"
      test "$(guest a "stat -c %u /srv/$c/sub /srv/$c/sub/entry" | sort -u)" = 65532 ||
        fail "entries the owner created in /srv/$c are not shown as its own"
    done

    # Without an owner a host share keeps showing the host user's tree as
    # root's, which is what guest root needs.
    if [ "$(uname -s)" = Linux ]; then
      test "$(guest a 'stat -c %u /srv/rooted')" = 0 || fail "an ownerless cache is no longer root's"
      if as_owner 'touch /srv/rooted/breach'; then
        fail "a non-root uid wrote into an ownerless cache"
      fi
    fi
  '';
}
