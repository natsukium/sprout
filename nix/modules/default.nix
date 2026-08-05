# Files such as entry.nix are base types, not modules. ./nixos is exported
# through flake.nixosModules instead: importing those guest modules here would
# evaluate them in the sprout VM-option module system.
let
  modulesIn =
    dir:
    let
      entries = builtins.readDir dir;
    in
    builtins.concatMap (name: if entries.${name} == "directory" then [ (dir + "/${name}") ] else [ ]) (
      builtins.attrNames entries
    );
in
modulesIn ./cache ++ modulesIn ./credential
