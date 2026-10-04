package main

import (
	"strings"
	"syscall"
	"testing"
)

// Linux exempts no address from the privileged-port rule, so its remedies must
// not include the macOS --bind 0.0.0.0 or launchd advice.
func TestPrivilegedPortHelpFitsTheHost(t *testing.T) {
	messages := func(goos string) map[string]string {
		return map[string]string{
			"forward":        forwardPrivilegedBindHelp(goos, 80),
			"route serve":    routePrivilegedBindError(goos, "127.0.0.1", 80, defaultRouteDomain).Error(),
			"missing router": missingRouterPort80Help(goos, 1024),
		}
	}
	for name, msg := range messages("linux") {
		if !strings.Contains(msg, "net.ipv4.ip_unprivileged_port_start") {
			t.Errorf("linux %s help %q does not name the sysctl", name, msg)
		}
		for _, darwinOnly := range []string{"0.0.0.0", "launchd"} {
			if strings.Contains(msg, darwinOnly) {
				t.Errorf("linux %s help %q offers the macOS-only %q", name, msg, darwinOnly)
			}
		}
	}
	for name, msg := range messages("darwin") {
		if strings.Contains(msg, "ip_unprivileged_port_start") {
			t.Errorf("darwin %s help %q offers the Linux-only sysctl", name, msg)
		}
	}
}

func TestPrivilegedBindRefusedFollowsTheHostRule(t *testing.T) {
	for _, c := range []struct {
		goos, bindHost string
		port, start    int
		err            error
		want           bool
	}{
		{"linux", "127.0.0.1", 1500, 2048, syscall.EACCES, true},
		{"linux", "0.0.0.0", 80, 1024, syscall.EACCES, true},
		{"linux", "127.0.0.1", 8080, 1024, syscall.EACCES, false},
		{"linux", "127.0.0.1", 80, 1024, syscall.EADDRINUSE, false},
		{"darwin", "127.0.0.1", 80, 1024, syscall.EACCES, true},
		{"darwin", "0.0.0.0", 80, 1024, syscall.EACCES, false},
	} {
		if got := privilegedBindRefused(c.goos, c.bindHost, c.port, c.start, c.err); got != c.want {
			t.Errorf("privilegedBindRefused(%s, %s, %d, start %d, %v) = %v, want %v", c.goos, c.bindHost, c.port, c.start, c.err, got, c.want)
		}
	}
}

// A Linux host whose threshold already admits :80 has nothing to fix there.
func TestMissingRouterHelpOmitsAnOpenPort80(t *testing.T) {
	if help := missingRouterPort80Help("linux", 0); help != "" {
		t.Errorf("help with ip_unprivileged_port_start=0 = %q, want none", help)
	}
}
