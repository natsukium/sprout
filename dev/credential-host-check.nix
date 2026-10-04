# Each host's bundle must record host-side credential scripts built for that
# host, through both the flake-parts module and lib.mkVM. The scripts are never
# built: the Darwin ones cannot be on a Linux checker.
{ inputs, lib }:
let
  hosts = [
    "aarch64-darwin"
    "aarch64-linux"
    "x86_64-linux"
  ];
  credentials = {
    gh.enable = true;
    aws-config.enable = true;
  };

  viaFlakeModule =
    (inputs.flake-parts.lib.mkFlake
      {
        inputs = {
          inherit (inputs) nixpkgs;
          self = {
            outPath = ./.;
            inputs = { inherit (inputs) nixpkgs; };
          };
        };
      }
      {
        systems = hosts;
        imports = [ inputs.self.flakeModules.default ];
        sprout.vms.check = { inherit credentials; };
      }
    ).sproutConfigurations;

  viaMkVM = lib.genAttrs hosts (hostSystem: {
    check = inputs.self.lib.mkVM {
      pkgs = inputs.nixpkgs.legacyPackages.${hostSystem};
      inherit credentials;
    };
  });

  # Pure evaluation cannot read a .drv file, so the builder reads it instead.
  # A plain path context makes the .drv itself an input; a deep (allOutputs)
  # context would instead require building every derivation in its closure.
  execDrv =
    exec:
    let
      drvPath = lib.head (lib.attrNames (builtins.getContext (lib.head exec)));
    in
    builtins.appendContext drvPath { ${drvPath}.path = true; };

  cases =
    lib.concatMap
      (
        { path, configurations }:
        lib.concatMap (
          hostSystem:
          map (c: "${path} ${hostSystem} ${c.name} ${execDrv c.exec}") (
            lib.filter (c: c.strategy == "materialize") configurations.${hostSystem}.check.manifest.credentials
          )
        ) hosts
      )
      [
        {
          path = "flakeModules.default";
          configurations = viaFlakeModule;
        }
        {
          path = "lib.mkVM";
          configurations = viaMkVM;
        }
      ];

  expected = 2 * lib.length hosts * lib.length (lib.attrNames credentials);
in
assert lib.assertMsg (lib.length cases == expected)
  "credential-host: expected ${toString expected} materialize execs, got:\n${lib.concatLines cases}";
pkgs:
pkgs.runCommand "credential-host-check" { cases = lib.concatStringsSep "\n" cases; } ''
  status=0
  while read -r path host name drv; do
    system=$(grep -o '("system","[^"]*")' "$drv" | cut -d '"' -f 4)
    if [ "$system" != "$host" ]; then
      echo "$path: $host bundle records $name's exec built for $system ($drv)" >&2
      status=1
    fi
  done <<<"$cases"
  if [ "$status" -ne 0 ]; then
    exit 1
  fi
  touch $out
''
