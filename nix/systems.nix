# The supported host set must agree with hostNixSystemFor in
# cmd/sprout/definition.go, or the CLI selects a slice the flake never exposes.
let
  guests = {
    aarch64-darwin = "aarch64-linux";
    aarch64-linux = "aarch64-linux";
    x86_64-linux = "x86_64-linux";
  };
  hypervisors = {
    aarch64-darwin = "vfkit";
    aarch64-linux = "qemu";
    x86_64-linux = "qemu";
  };
  hosts = builtins.attrNames guests;

  lookup =
    kind: mapping: hostSystem:
    mapping.${hostSystem}
      or (throw "sprout: host system ${hostSystem} has no ${kind} (supported: ${builtins.concatStringsSep ", " hosts})");
in
{
  inherit hosts;
  guestFor = lookup "guest" guests;
  hypervisorFor = lookup "runner" hypervisors;
}
