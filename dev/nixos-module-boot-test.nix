# Real guests booted, restarted, woken and stopped through the NixOS module.
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
      memorySize = 4096;
      cores = 2;
      diskSize = 4096;
    };
    users.users.alice.isNormalUser = true;
    environment.systemPackages = [
      sprout
      pkgs.curl
    ];
    nix.settings.experimental-features = [
      "nix-command"
      "flakes"
    ];

    services.sprout = {
      enable = true;
      user = "alice";
      route.enable = true;
      instances.runner = {
        vcpu = 1;
        mem = 1024;
        idle.action = "none";
      };
      instances.woken = {
        vcpu = 1;
        mem = 1024;
        idle.action = "none";
        autoStart = false;
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

    with subtest("a request wakes a stopped instance inside the router's unit"):
        machine.succeed("systemctl start sprout-woken.service")
        machine.wait_until_succeeds("journalctl -u sprout-woken.service -o cat | grep -q '^VM ready'")
        machine.succeed("systemctl stop sprout-woken.service")
        machine.succeed("curl -s -o /dev/null -H 'Host: woken.sprout.localhost' http://127.0.0.1/")
        machine.wait_until_succeeds("su - alice -c 'sprout exec --instance woken -- true'")
        cgroups = machine.succeed(
            "ps -eo pid=,comm= | awk '/qemu/ {print $1}' | xargs -I{} cat /proc/{}/cgroup"
        )
        assert "sprout-route.service" in cgroups, f"the woken runner is not the router's: {cgroups}"

    woken_dir = machine.succeed(
        "dirname $(grep -l '\"woken\"' /home/alice/.local/state/sprout/instances/*/instance.json)"
    ).strip()

    with subtest("host shutdown powers a router-woken guest off"):
        machine.fail(f"grep -q 'System Power Off' {woken_dir}/console.log")
        machine.shutdown()
        machine.start()
        machine.wait_for_unit("multi-user.target")
        machine.succeed(f"grep -q 'System Power Off' {woken_dir}/console.log")
        machine.succeed("journalctl -b -1 -u sprout-route-instances.service -o cat | grep -q 'instance \"woken\" stopped'")

    with subtest("host shutdown right after a wake still stops the booting guest"):
        machine.succeed("curl -s -o /dev/null -H 'Host: woken.sprout.localhost' http://127.0.0.1/")
        machine.shutdown()
        machine.start()
        machine.wait_for_unit("multi-user.target")
        machine.succeed("journalctl -b -1 -u sprout-route-instances.service -o cat | grep -q 'instance \"woken\" stopped'")
  '';
}
