# sprout documentation

Organized by [Diátaxis](https://diataxis.fr): what you are trying to do
decides where to look.

| You are… | Start here |
| --- | --- |
| booting your first VM | [Getting started](tutorials/getting-started.md) |
| accomplishing a task | [How-to guides](#how-to) |
| looking a fact up | [Reference](#reference) |
| understanding why | [Explanation](#explanation) |
| a coding agent driving sprout | [Agent skill](../skills/sprout/SKILL.md) |

## How-to

Run and reach instances:

- [Compare branches side by side](how-to/compare-branches.md)
- [Reach instances by name](how-to/route.md)
- [Forward ports](how-to/forward-ports.md)

Carry state between instances:

- [Snapshot and fork a volume](how-to/snapshots.md)

Bring host resources into the guest:

- [Project host credentials](how-to/project-credentials.md)
- [Share build caches](how-to/share-caches.md)
- [Reuse the project devShell in the guest](how-to/reuse-the-devshell.md)

Operate beyond the dev loop:

- [Run a supervised instance under launchd](how-to/run-as-daemon.md)
- [Emulate a foreign architecture](how-to/emulate-foreign-architectures.md)

## Reference

- [CLI](reference/cli.md)
- [Configuration](reference/configuration.md)
- [Instance state](reference/instance-state.md)
- [Compatibility and release policy](reference/compatibility.md)

## Explanation

- [Instance identity](explanation/instances.md)
- [Architecture: Nix defines, Go executes](explanation/architecture.md)
