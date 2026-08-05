package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newDoctorCmd() *cobra.Command {
	var build bool
	cmd := &cobra.Command{
		Use:     "doctor",
		Short:   "Check Nix, the Linux builder, virtualization, ssh",
		GroupID: groupSetup,
		Args:    usageArgs(cobra.NoArgs),
		RunE:    func(_ *cobra.Command, _ []string) error { return cmdDoctor(build) },
	}
	cmd.Flags().BoolVar(&build, "build", false, "also run a real (trivial) Linux build through the configured builder")
	return cmd
}

func cmdDoctor(build bool) error {
	checks := []doctorCheck{
		{"platform", checkPlatform},
		{"nix", checkNix},
		{"flakes", checkFlakes},
		{"linux builder", checkLinuxBuilder},
		{"virtualization", checkVirtualization},
		{"ssh", checkSSH},
	}
	if build {
		checks = append(checks, doctorCheck{"linux build", checkLinuxBuild})
	}

	failed := 0
	for _, c := range checks {
		detail, err := c.run()
		if err != nil {
			failed++
			fmt.Printf("✗ %-15s %v\n", c.name, err)
			continue
		}
		fmt.Printf("✓ %-15s %s\n", c.name, detail)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d checks failed", failed, len(checks))
	}
	fmt.Println("\nAll checks passed. `sprout up` should work in a flake with a sprout.vms definition.")
	return nil
}

type doctorCheck struct {
	name string
	run  func() (string, error)
}

func checkPlatform() (string, error) {
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("sprout boots VMs through Virtualization.framework and only runs on macOS (this is %s)", runtime.GOOS)
	}
	if runtime.GOARCH != "arm64" {
		return "", fmt.Errorf("sprout builds aarch64-linux guests and only runs on Apple Silicon (this Mac is %s)", runtime.GOARCH)
	}
	return fmt.Sprintf("macOS %s", runtime.GOARCH), nil
}

func checkNix() (string, error) {
	out, err := exec.Command("nix", "--version").Output()
	if err != nil {
		return "", fmt.Errorf("`nix` not found on PATH — install it from https://nixos.org/download or https://determinate.systems")
	}
	return strings.TrimSpace(string(out)), nil
}

func checkFlakes() (string, error) {
	features, err := nixConfigShow("experimental-features")
	if err != nil {
		// `nix config show` is itself gated behind nix-command, so this
		// failure already is the diagnosis.
		return "", fmt.Errorf("flakes are not enabled — add `experimental-features = nix-command flakes` to ~/.config/nix/nix.conf (or /etc/nix/nix.conf)")
	}
	have := strings.Fields(features)
	for _, want := range []string{"nix-command", "flakes"} {
		if !slices.Contains(have, want) {
			return "", fmt.Errorf("experimental-features is %q — add `experimental-features = nix-command flakes` to your nix.conf", features)
		}
	}
	return "nix-command flakes", nil
}

// Reads configuration rather than building: a config-level "no" is certain
// and immediate. `doctor --build` does the end-to-end proof.
func checkLinuxBuilder() (string, error) {
	system := guestNixSystem()
	if builders, err := nixConfigShow("builders"); err == nil {
		if b := strings.TrimSpace(builders); b != "" && hasBuilderEntries(b) {
			return fmt.Sprintf("builders = %s", summarize(b)), nil
		}
	}
	if platforms, err := nixConfigShow("extra-platforms"); err == nil {
		if slices.Contains(strings.Fields(platforms), system) {
			return fmt.Sprintf("extra-platforms includes %s", system), nil
		}
	}
	return "", fmt.Errorf("no %s builder — the guest is a Linux NixOS closure a Mac cannot build natively; run `nix run nixpkgs#darwin.linux-builder`, or set nix-darwin's `nix.linux-builder` option (see examples/README.md#building-on-apple-silicon)", system)
}

// The setting may be a literal list or an `@file` indirection, and an
// existing-but-empty machines file must not count as a builder.
func hasBuilderEntries(builders string) bool {
	for _, entry := range strings.Split(builders, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if file, ok := strings.CutPrefix(entry, "@"); ok {
			data, err := os.ReadFile(file)
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line != "" && !strings.HasPrefix(line, "#") {
					return true
				}
			}
			continue
		}
		return true
	}
	return false
}

func checkVirtualization() (string, error) {
	out, err := exec.Command("sysctl", "-n", "kern.hv_support").Output()
	if err != nil || strings.TrimSpace(string(out)) != "1" {
		return "", fmt.Errorf("Virtualization.framework not supported (kern.hv_support != 1) — sprout needs Apple Silicon hardware virtualization; VMs cannot boot on this machine")
	}
	return "kern.hv_support = 1", nil
}

func checkSSH() (string, error) {
	path, err := exec.LookPath("ssh")
	if err != nil {
		return "", fmt.Errorf("`ssh` not found on PATH — sprout reaches guests through the system ssh client")
	}
	return path, nil
}

// Bounded because the classic failure of a misconfigured builder is not an
// error but a hang.
func checkLinuxBuild() (string, error) {
	system := guestNixSystem()
	expr := fmt.Sprintf(`derivation { name = "sprout-doctor"; system = %q; builder = "/bin/sh"; args = [ "-c" "echo ok > $out" ]; }`, system)
	ctx, cancel := context.WithTimeout(context.Background(), linuxBuildTimeout)
	defer cancel()
	if err := exec.CommandContext(ctx, "nix", "build", "--no-link", "--expr", expr).Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("a trivial %s build did not finish within %s — the configured builder is unreachable or wedged", system, linuxBuildTimeout)
		}
		return "", fmt.Errorf("a trivial %s build failed — the configured builder is not usable (run `nix build --expr '…'` by hand for the full error)", system)
	}
	return fmt.Sprintf("built a trivial %s derivation", system), nil
}

const linuxBuildTimeout = 2 * time.Minute

func nixConfigShow(setting string) (string, error) {
	out, err := exec.Command("nix", "config", "show", setting).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func guestNixSystem() string {
	return nixArch() + "-linux"
}

func summarize(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 60 {
		return s[:57] + "…"
	}
	return s
}
