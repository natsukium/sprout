# The scope boundary, end to end: a project cache is reachable from every
# instance of one clone and from no other clone, while a shared cache in the
# same guest crosses both. Uses two fixture repositories because the driver's
# scratch directory is not one, and project keying reads the git-common-dir.
{ ... }:
{
  vm.caches = {
    proj = {
      enable = true;
      guestPath = "/root/.cache/proj";
      scope = "project";
    };
    wide = {
      enable = true;
      guestPath = "/root/.cache/wide";
      scope = "shared";
    };
  };
  testScript = ''
    git init -q "$SPROUT_TEST_DIR/repo1"
    git init -q "$SPROUT_TEST_DIR/repo2"

    cd "$SPROUT_TEST_DIR/repo1"
    up a
    up b
    cd "$SPROUT_TEST_DIR/repo2"
    up c

    guest a 'touch /root/.cache/proj/marker /root/.cache/wide/marker'
    guest b 'test -f /root/.cache/proj/marker'
    guest c 'test ! -f /root/.cache/proj/marker'
    guest c 'test -f /root/.cache/wide/marker'

    # Each clone gets its own tree, labelled by the repository directory so
    # the listing stays readable without a per-cache record to resolve.
    test "$(find "$XDG_CACHE_HOME/sprout" -type d -name proj | wc -l | tr -d ' ')" = 2
    find "$XDG_CACHE_HOME/sprout" -type d -name 'repo1-*' | grep -q .
    find "$XDG_CACHE_HOME/sprout" -type d -name 'repo2-*' | grep -q .
  '';
}
