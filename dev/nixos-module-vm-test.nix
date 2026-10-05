# Stand-ins replace the guests, which would need nested KVM here; real boots
# are nixos-module-boot-test.nix.
{ inputs }:
pkgs:
let
  sprout = inputs.self.packages.${pkgs.stdenv.hostPlatform.system}.sprout;

  # Stops its runner itself after a slow poweroff, as `sprout up` does; a
  # runner SIGTERMed by anyone else logs it.
  standIn = pkgs.writeShellScript "sprout-up-stand-in" ''
    log="$HOME/instance.log"
    echo "start $(id -un) $1" >>"$log"
    for tool in git ssh nix ps; do
      command -v "$tool" >/dev/null || { echo "missing $tool" >>"$log"; exit 1; }
    done
    ${pkgs.util-linux}/bin/setsid ${pkgs.bash}/bin/bash -c '
      trap "echo runner-signalled-directly >>\"$HOME/instance.log\"; exit 0" TERM
      while :; do sleep 0.2 & wait $!; done
    ' &
    runner=$!
    trap 'sleep 5; kill -9 "$runner"; wait "$runner"; echo "stopped $1" >>"$log"; exit 0' TERM
    wait
  '';
  standInFor = priority: arg: {
    serviceConfig.ExecStart = pkgs.lib.mkOverride priority "${standIn} ${arg}";
  };

  # A woken daemon: in the router's cgroup, with no unit of its own.
  wokenStandIn = pkgs.writeShellScript "sprout-woken-stand-in" ''
    echo $$ >"$HOME/woken.pid"
    trap '
      if [ -e "$HOME/swept" ]; then echo woken-stopped-by-sweep; else echo woken-signalled-by-another; fi >>"$HOME/woken.log"
      exit 0
    ' TERM
    while :; do sleep 0.2 & wait $!; done
  '';
  sweepStandIn = pkgs.writeShellScript "sprout-stop-all-stand-in" ''
    state() { systemctl is-active "$1" || true; }
    echo "sweep router=$(state sprout-route.service) network=$(state network.target) nix=$(state nix-daemon.socket) graceful=$(state sprout-graceful.service)" >>"$HOME/woken.log"
    touch "$HOME/swept"
    pid=$(cat "$HOME/woken.pid")
    kill -TERM "$pid"
    while kill -0 "$pid" 2>/dev/null; do sleep 0.2; done
  '';
in
pkgs.testers.runNixOSTest {
  name = "sprout-nixos-module";

  nodes.machine =
    { lib, ... }:
    {
      imports = [ inputs.self.nixosModules.default ];
      users.users.alice.isNormalUser = true;
      environment.systemPackages = [ pkgs.curl ];

      services.sprout = {
        enable = true;
        user = "alice";
        route.enable = true;
        instances.graceful = { };
        instances.broken = { };
      };

      systemd.services.sprout-graceful = standInFor 50 "v1";
      systemd.services.sprout-broken.serviceConfig.ExecStart =
        lib.mkForce "${sprout}/bin/sprout up --foreground --bundle /nonexistent-bundle --instance broken";
      systemd.services.sprout-route-instances.serviceConfig.ExecStop = lib.mkForce sweepStandIn;

      specialisation.rebuilt.configuration.systemd.services = {
        sprout-graceful = standInFor 40 "v2";
        sprout-broken.enable = lib.mkForce false;
      };
    };

  testScript = ''
    instance_log = "/home/alice/instance.log"

    def main_pid(unit):
        return machine.succeed(f"systemctl show -p MainPID --value {unit}").strip()

    machine.wait_for_unit("multi-user.target")

    with subtest("the router starts on demand, as alice, on the port systemd bound"):
        machine.wait_for_unit("sprout-route.socket")
        machine.fail("systemctl is-active sprout-route.service")
        for addr in ["127.0.0.1", "[::1]"]:
            code = machine.succeed(
                f"curl -s -o /dev/null -w '%{{http_code}}' -H 'Host: nope.sprout.localhost' http://{addr}/"
            )
            assert code == "404", f"router on {addr} answered {code}, want its 404 for an unknown instance"
        pid = main_pid("sprout-route.service")
        assert machine.succeed(f"ps -o user= -p {pid}").strip() == "alice"
        cap = machine.succeed(f"awk '/^CapEff/ {{print $2}}' /proc/{pid}/status").strip()
        assert int(cap, 16) == 0, f"router holds capabilities {cap}"
        machine.succeed(f"tr '\\0' '\\n' </proc/{pid}/environ | grep -q '^PATH=.*procps'")

    with subtest("a router restart keeps serving through the same socket"):
        machine.succeed("systemctl restart sprout-route.service")
        code = machine.succeed("curl -s -o /dev/null -w '%{http_code}' -H 'Host: nope.sprout.localhost' http://127.0.0.1/")
        assert code == "404", code

    with subtest("what the router woke outlives the router's restarts"):
        machine.succeed("su - alice -c '${pkgs.util-linux}/bin/setsid ${wokenStandIn} >/dev/null 2>&1 &'")
        machine.wait_until_succeeds("test -s /home/alice/woken.pid")
        woken = machine.succeed("cat /home/alice/woken.pid").strip()
        cgroup = machine.succeed("systemctl show -p ControlGroup --value sprout-route.service").strip()
        machine.succeed(f"echo {woken} > /sys/fs/cgroup{cgroup}/cgroup.procs")
        machine.succeed("systemctl restart sprout-route.service")
        machine.succeed(f"kill -0 {woken}")
        machine.succeed("systemctl is-active sprout-route-instances.service")
        machine.fail("test -e /home/alice/woken.log")

    with subtest("an instance starts at boot as alice with the host tools on PATH"):
        machine.wait_until_succeeds(f"grep -qx 'start alice v1' {instance_log}")
        machine.fail(f"grep -q missing {instance_log}")

    with subtest("stopping signals only sprout and waits out its graceful stop"):
        machine.succeed("systemctl is-active sprout-graceful.service")
        machine.succeed("systemctl stop sprout-graceful.service")
        machine.succeed(f"grep -qx 'stopped v1' {instance_log}")
        machine.fail(f"grep -q runner-signalled-directly {instance_log}")
        machine.succeed("systemctl start sprout-graceful.service")
        machine.wait_until_succeeds(f"test $(grep -cx 'start alice v1' {instance_log}) -eq 2")

    with subtest("a failed boot exits non-zero and is retried, not dropped"):
        machine.wait_until_succeeds("systemctl show -p SubState --value sprout-broken.service | grep -qx auto-restart")
        machine.succeed("journalctl -u sprout-broken.service | grep -q nonexistent-bundle")
        assert machine.succeed("systemctl show -p ExecMainStatus --value sprout-broken.service").strip() != "0"

    with subtest("a rebuild reports an instance that fails to start"):
        status, out = machine.execute("/run/current-system/bin/switch-to-configuration test 2>&1")
        assert status != 0 and "sprout-broken.service" in out, f"rebuild exited {status}:\n{out}"

    with subtest("a rebuild that changes the instance restarts it gracefully"):
        machine.succeed("/run/current-system/specialisation/rebuilt/bin/switch-to-configuration test")
        machine.wait_until_succeeds(f"grep -qx 'start alice v2' {instance_log}")
        assert machine.succeed(f"grep -cx 'stopped v1' {instance_log}").strip() == "2"
        machine.fail(f"grep -q runner-signalled-directly {instance_log}")
        machine.succeed(f"kill -0 {woken}")
        machine.fail("test -e /home/alice/woken.log")

    with subtest("host shutdown stops supervised and router-woken instances gracefully"):
        machine.succeed("curl -s -o /dev/null -H 'Host: nope.sprout.localhost' http://127.0.0.1/")
        machine.shutdown()
        machine.start()
        machine.wait_for_unit("multi-user.target")
        machine.succeed(f"grep -qx 'stopped v2' {instance_log}")
        machine.fail(f"grep -q runner-signalled-directly {instance_log}")
        machine.succeed("grep -qx 'sweep router=inactive network=active nix=active graceful=inactive' /home/alice/woken.log")
        machine.succeed("grep -qx woken-stopped-by-sweep /home/alice/woken.log")
        machine.fail("grep -q woken-signalled-by-another /home/alice/woken.log")
  '';
}
