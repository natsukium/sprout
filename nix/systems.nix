# The supported host set must agree with hostNixSystemFor in
# cmd/sprout/definition.go, and each host's backends with backendKinds in
# cmd/sprout/backend.go, or the CLI selects a slice the flake never exposes.
let
  table = {
    aarch64-darwin = {
      guest = "aarch64-linux";
      backends = [ "vfkit" ];
      defaultBackend = "vfkit";
    };
    aarch64-linux = {
      guest = "aarch64-linux";
      backends = [ "qemu" ];
      defaultBackend = "qemu";
    };
    x86_64-linux = {
      guest = "x86_64-linux";
      backends = [ "qemu" ];
      defaultBackend = "qemu";
    };
  };
  hosts = builtins.attrNames table;

  entryFor =
    hostSystem:
    table.${hostSystem}
      or (throw "sprout: host system ${hostSystem} is not supported (supported: ${builtins.concatStringsSep ", " hosts})");
in
{
  inherit table hosts;
  guestFor = hostSystem: (entryFor hostSystem).guest;

  # `sprout.vms` is evaluated once for every host, so "auto" can only be
  # resolved here, per host bundle.
  resolveBackend =
    hostSystem: requested:
    let
      entry = entryFor hostSystem;
    in
    if requested == "auto" then
      entry.defaultBackend
    else if builtins.elem requested entry.backends then
      requested
    else
      throw "sprout: backend \"${requested}\" cannot run on ${hostSystem}; it allows ${builtins.concatStringsSep ", " entry.backends} (or \"auto\")";
}
