package main

import (
	"fmt"
	"os"
)

// writeInstanceEnv projects the instance's identity into the guest through
// the data share (guest path /run/sprout/instance.env), so one bundle serves
// every branch and a guest needing a per-instance value reads it at boot
// instead of baking it into the image.
//
// Lines are KEY='value' so the file works both sourced by a shell and read
// by systemd's EnvironmentFile=. The shell-style escape for an embedded
// single quote is not systemd syntax, but such a quote only appears if the
// user named a branch with one.
//
// label is passed in rather than derived here, so the guest gets the host's
// own answer instead of a second implementation of the rule.
func writeInstanceEnv(path string, inst *Instance, label string) error {
	content := fmt.Sprintf(
		"SPROUT_INSTANCE_ID=%s\nSPROUT_INSTANCE_NAME=%s\nSPROUT_INSTANCE_LABEL=%s\nSPROUT_DEFINITION=%s\n",
		shellQuote(inst.ID), shellQuote(inst.Name), shellQuote(label), shellQuote(inst.Definition),
	)
	return os.WriteFile(path, []byte(content), 0o644)
}
