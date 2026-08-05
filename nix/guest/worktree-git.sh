# Make a linked git worktree usable from inside the guest.
#
# `workspace = true` shares only the worktree directory, and a linked
# worktree's `.git` is a *file* naming its admin directory under the main
# repository — a host path outside the share. Git in the guest follows it to
# nothing and reports /workspace as not a repository at all. The main
# repository's common gitdir comes in as a second share; this recreates the
# host's spelling of both paths as symlinks onto the two shares, so git
# resolves the files it already wrote without either being rewritten.
#
# The two paths are read out of the git files themselves rather than handed
# in by the host, because git committed to a spelling when it wrote them and
# only that spelling is ever followed: sprout's own record of the pair is
# symlink-resolved on one side and not the other, and where the repository
# lives under a symlinked prefix (/tmp -> /private/tmp on macOS) the two
# disagree. Reading them back also keeps the guest self-contained — no
# per-instance channel to plumb, so `start` needs no host-side work.
#
# Usage: sprout-worktree-git [WORKSPACE] [GIT_COMMON_MOUNT]

set -eu

workspace=${1:-/workspace}
gitcommon=${2:-/run/sprout-git}

# A primary checkout keeps a real .git directory inside the share, so it is
# already whole; only the file form needs bridging.
[ -f "$workspace/.git" ] || exit 0

line=""
read -r line <"$workspace/.git" || true
case $line in
'gitdir: '*) gitdir=${line#gitdir: } ;;
*) exit 0 ;;
esac
# A relative gitdir resolves inside the share on its own.
case $gitdir in
/*) ;;
*) exit 0 ;;
esac

# Only a worktree admin dir (<common>/worktrees/<name>) can be bridged this
# way. A submodule's .git file has the same shape but names
# <super>/.git/modules/<name>, whose worktree is found through core.worktree
# rather than through the reverse pointer below, so leave it to the warning.
admin=${gitdir%/*}
[ "${admin##*/}" = worktrees ] || exit 0
common=${admin%/*}
[ -n "$common" ] || exit 0

# The guest root is a tmpfs, so materializing a host-shaped path costs
# nothing and vanishes with the VM. A repository under /var is the exception
# — that lands on the volume that survives `stop` — so every step here has to
# tolerate being re-run against what a previous boot left behind.
parent=${common%/*}
mkdir -p "${parent:-/}"
ln -sfn "$gitcommon" "$common"

# <admin>/gitdir is git's reverse pointer, naming the worktree that owns the
# admin dir; `git worktree list` and the prune `git gc` runs both read it.
# Left dangling, the guest reports its own worktree as prunable.
[ -r "$gitdir/gitdir" ] || exit 0
back=""
read -r back <"$gitdir/gitdir" || true
[ "${back##*/}" = .git ] || exit 0
worktree=${back%/*}
[ -n "$worktree" ] || exit 0
parent=${worktree%/*}
mkdir -p "${parent:-/}"
ln -sfn "$workspace" "$worktree"
