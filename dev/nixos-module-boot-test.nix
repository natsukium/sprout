# A real guest booted, restarted and stopped through the NixOS module's unit.
# The guest runs inside the test VM, so the builder must offer nested KVM; that
# keeps this out of `checks` (see nixos-module-vm-test.nix for the part that
# needs none). Run it with `nix build .#legacyPackages.x86_64-linux.nixosTests.module-boot`.
{ inputs }:
pkgs:
let
  sprout = inputs.self.packages.${pkgs.stdenv.hostPlatform.system}.sprout;
in
pkgs.testers.runNixOSTest {
  name = "sprout-nixos-module-boot";

  nodes.machine = {
    imports = [ inputs.self.nixosModules.default ];
    virtualisation = {
      memorySize = 3072;
      cores = 2;
      diskSize = 4096;
    };
    users.users.alice.isNormalUser = true;
    environment.systemPackages = [ sprout ];
    nix.settings.experimental-features = [
      "nix-command"
      "flakes"
    ];

    services.sprout = {
      enable = true;
      user = "alice";
      instances.runner = {
        vcpu = 1;
        mem = 1024;
        idle.action = "none";
      };
    };
  };

  testScript = ''
    def ready_count():
        return int(machine.succeed("journalctl -u sprout-runner.service -o cat | grep -c '^VM ready' || true").strip())

    def guest(cmd):
        return machine.succeed(f"su - alice -c 'sprout exec --instance runner -- {cmd}'")

    machine.wait_for_unit("multi-user.target")

    with subtest("the instance boots at startup as alice"):
        machine.wait_until_succeeds("test $(journalctl -u sprout-runner.service -o cat | grep -c '^VM ready') -ge 1")
        guest("true")
        pid = machine.succeed("systemctl show -p MainPID --value sprout-runner.service").strip()
        assert machine.succeed(f"ps -o user= -p {pid}").strip() == "alice"

    with subtest("a restart powers the guest off and boots it again"):
        before = ready_count()
        machine.succeed("systemctl restart sprout-runner.service")
        machine.wait_until_succeeds(f"test $(journalctl -u sprout-runner.service -o cat | grep -c '^VM ready') -gt {before}")
        guest("true")

    with subtest("a stop powers the guest off rather than cutting it"):
        machine.succeed("systemctl stop sprout-runner.service")
        machine.succeed("journalctl -u sprout-runner.service -o cat | grep -q 'instance \"runner\" stopped'")
        machine.succeed("grep -rq 'System Power Off' /home/alice/.local/state/sprout/instances")
        machine.fail("grep -rq 'forcing stop' /home/alice/.local/state/sprout/instances")
        machine.fail("ps -eo comm= | grep -q qemu")
  '';
}
