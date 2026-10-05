package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

var errPrivilegedBind = errors.New("a non-root bind on a privileged port was refused")

// Linux lets the administrator move the threshold off 1024, and a raised one
// would otherwise leave its refusals looking like an unrelated EACCES.
func unprivilegedPortStart(goos string) int {
	if goos != "linux" {
		return 1024
	}
	data, err := os.ReadFile("/proc/sys/net/ipv4/ip_unprivileged_port_start")
	if err != nil {
		return 1024
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 1024
	}
	return n
}

// macOS exempts a non-root wildcard bind from the rule, and Linux exempts no
// address.
func privilegedBindRefused(goos, bindHost string, port, unprivilegedStart int, err error) bool {
	if goos == "darwin" && bindHost == "0.0.0.0" {
		return false
	}
	return port < unprivilegedStart && errors.Is(err, syscall.EACCES)
}

func unprivilegedPortStartFix(port int) string {
	return fmt.Sprintf("lower net.ipv4.ip_unprivileged_port_start (sudo sysctl -w net.ipv4.ip_unprivileged_port_start=%d), which lets every local user bind from %d up", port, port)
}

func forwardPrivilegedBindHelp(goos string, port int) string {
	if goos == "linux" {
		return "forward through an unprivileged host port (PORT:GUESTPORT picks both sides), or " + unprivilegedPortStartFix(port)
	}
	return "macOS allows a non-root bind below 1024 only on 0.0.0.0, so rerun with --bind 0.0.0.0 to bind all interfaces instead"
}

func routePrivilegedBindError(goos, bindHost string, port int, domain string) error {
	if goos == "linux" {
		return fmt.Errorf("cannot bind %s:%d: Linux refuses a non-root bind below net.ipv4.ip_unprivileged_port_start. Choose one:\n"+
			"  • an unprivileged port:  sprout route serve --port 8080     (URLs then carry it: http://<name>.%s:8080/)\n"+
			"  • %s\n"+
			"  • a systemd socket that binds :%d for you (services.sprout.route on NixOS, see docs/how-to/route.md)",
			bindHost, port, domain, unprivilegedPortStartFix(port), port)
	}
	return fmt.Errorf("cannot bind %s:%d: macOS forbids a non-root bind on a privileged port (<1024) against a specific address. Choose one:\n"+
		"  • an unprivileged port:  sprout route serve --port 8080     (URLs then carry it: http://<name>.%s:8080/)\n"+
		"  • all interfaces:        sprout route serve --bind 0.0.0.0  (allowed without root, but reachable from your LAN — every instance's every port, by Host header)\n"+
		"  • a launchd daemon that binds :%d for you (services.sprout.route, see docs/how-to/route.md)",
		bindHost, port, domain, port)
}

func missingRouterPort80Help(goos string, unprivilegedStart int) string {
	if goos == "linux" {
		if unprivilegedStart <= 80 {
			return ""
		}
		return "Linux refuses a non-root bind of :80 here, so that needs `--port 8080` (and `sprout open --port 8080`), the systemd socket in docs/how-to/run-as-daemon.md, or to " + unprivilegedPortStartFix(80)
	}
	return "macOS refuses a non-root bind of :80, so that needs either `--port 8080` (and `sprout open --port 8080`) or the launchd job in docs/how-to/run-as-daemon.md"
}
