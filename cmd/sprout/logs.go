package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newLogsCmd() *cobra.Command {
	var (
		follow bool
		lines  int
	)
	cmd := &cobra.Command{
		Use:     "logs",
		Short:   "Show runner/console logs",
		GroupID: groupDaily,
		Args:    usageArgs(noPositionals),
	}
	selector := addInstanceFlag(cmd)
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow console.log as new output arrives")
	cmd.Flags().IntVarP(&lines, "lines", "n", 80, "number of trailing lines to show per log")
	cmd.RunE = func(_ *cobra.Command, _ []string) error { return cmdLogs(*selector, follow, lines) }
	return cmd
}

func cmdLogs(selector string, follow bool, lines int) error {
	// resolveAndLoad, not bare identity resolution: an absent instance must
	// fail like `exec` does, not exit 0 with nothing printed.
	id, _, dir, err := resolveAndLoad(selector)
	if err != nil {
		return err
	}
	printed := false
	for _, log := range []string{"runner.log", "console.log"} {
		path := filepath.Join(dir, log)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		fmt.Printf("==> %s <==\n", path)
		printTail(string(data), lines)
		printed = true
	}
	if !follow {
		if !printed {
			return fmt.Errorf("no runner or console logs for instance %q yet; build output goes to %s", id.Display(), upLogPath(dir))
		}
		return nil
	}
	return followLog(consoleLogPath(dir))
}

func printTail(data string, n int) {
	lines := strings.Split(strings.TrimRight(data, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	fmt.Println(strings.Join(lines, "\n"))
}

// Reopens whenever the file shrinks: console.log is recreated on every
// `sprout up`, so a `logs -f` left running across a reboot picks up the new
// output instead of hanging on a deleted inode.
func followLog(path string) error {
	fmt.Printf("==> following %s (Ctrl-C to stop) <==\n", path)
	var offset int64
	for {
		f, err := os.Open(path)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			time.Sleep(time.Second)
			continue
		}
		if info.Size() < offset {
			offset = 0
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return err
		}
		n, _ := io.Copy(os.Stdout, f)
		offset += n
		f.Close()
		time.Sleep(500 * time.Millisecond)
	}
}
