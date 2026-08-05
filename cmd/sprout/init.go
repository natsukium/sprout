package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// The smallest flake that boots, the same text as the getting-started
// tutorial's: one that has drifted from the tutorial is worse than none,
// since a reader cannot tell which is current.
const initFlakeTemplate = `{
  inputs.sprout.url = "github:natsukium/sprout";
  inputs.nixpkgs.follows = "sprout/nixpkgs";
  inputs.flake-parts.follows = "sprout/flake-parts";

  outputs =
    inputs@{ flake-parts, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {
      systems = [ "aarch64-darwin" ];
      imports = [ inputs.sprout.flakeModules.default ];

      sprout.vms.%s = {
        vcpu = 4;
        mem = "8GiB";
        workspace = true; # mount the git toplevel at /workspace
      };
    };
}
`

func newInitCmd() *cobra.Command {
	var def string
	cmd := &cobra.Command{
		Use:     "init",
		Short:   "Write a flake.nix declaring one VM",
		GroupID: groupSetup,
		Long: `Write a flake.nix with a single sprout VM definition, so ` + "`sprout up`" + ` has something
to build.

This is the smallest configuration that boots. Edit it afterwards — vcpu, mem,
guest modules, credentials, caches — see docs/reference/configuration.md.

To add sprout to a flake you already have, this command is not what you want: it
refuses to overwrite one. Copy the sprout.vms block out of the tutorial instead
(docs/tutorials/getting-started.md), which also covers flakes that do not use
flake-parts.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(_ *cobra.Command, _ []string) error { return cmdInit(def) },
	}
	cmd.Flags().StringVar(&def, "vm", "dev", "name of the VM definition to declare (sprout.vms.<name>)")
	return cmd
}

func cmdInit(def string) error {
	if err := validateDefinitionName(def); err != nil {
		return err
	}
	// O_EXCL rather than stat-then-write: the file this refuses to clobber
	// holds every input pin a project builds from, so the check has to be the
	// write itself.
	f, err := os.OpenFile("flake.nix", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("flake.nix already exists here; add a sprout.vms definition to it instead — see docs/tutorials/getting-started.md")
		}
		return err
	}
	defer f.Close()

	if _, err := fmt.Fprintf(f, initFlakeTemplate, def); err != nil {
		return err
	}
	fmt.Printf("wrote flake.nix declaring sprout.vms.%s\n", def)
	fmt.Println("\nIn a Git repository, stage the file so Nix can read it:\n  git add flake.nix")
	fmt.Println("\nBoot it with:\n  sprout up")
	return nil
}

// A name needing quotes would produce a flake.nix that does not parse, and
// the failure would surface from `nix build` long after the typo.
func validateDefinitionName(name string) error {
	if name == "" {
		return usagef("--vm needs a name")
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case i > 0 && (r >= '0' && r <= '9' || r == '-' || r == '\''):
		default:
			return usagef("%q is not a usable definition name: use letters, digits, dash and underscore, starting with a letter", name)
		}
	}
	return nil
}

func initHint() string {
	return strings.TrimSpace("This directory has no flake.nix. Write one with:\n  sprout init")
}
