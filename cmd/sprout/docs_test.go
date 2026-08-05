package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

type markdownBlock struct {
	path     string
	line     int
	language string
	body     string
}

func markdownBlocks(t *testing.T) []markdownBlock {
	t.Helper()

	var blocks []markdownBlock
	for _, root := range []string{"../../README.md", "../../docs", "../../examples", "../../skills"} {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "vendor" {
					return filepath.SkipDir
				}
				return nil
			}
			if filepath.Ext(path) != ".md" {
				return nil
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			lines := strings.Split(string(data), "\n")
			for i := 0; i < len(lines); i++ {
				if !strings.HasPrefix(lines[i], "```") {
					continue
				}
				language := strings.TrimSpace(strings.TrimPrefix(lines[i], "```"))
				start := i + 2
				i++
				var body []string
				for ; i < len(lines) && lines[i] != "```"; i++ {
					body = append(body, lines[i])
				}
				if i == len(lines) {
					return fmt.Errorf("%s:%d: unterminated code fence", path, start-1)
				}
				blocks = append(blocks, markdownBlock{
					path:     path,
					line:     start,
					language: language,
					body:     strings.Join(body, "\n"),
				})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return blocks
}

func TestDocumentedNixExamplesParse(t *testing.T) {
	nixInstantiate, err := exec.LookPath("nix-instantiate")
	if err != nil {
		t.Skip("nix-instantiate is not available")
	}

	for _, block := range markdownBlocks(t) {
		if block.language != "nix" {
			continue
		}
		block := block
		t.Run(fmt.Sprintf("%s:%d", block.path, block.line), func(t *testing.T) {
			expression := block.body
			if !nixBlockIsExpression(expression) {
				expression = "{\n" + expression + "\n}"
			}
			// Fragment examples inherit these names from their surrounding flake
			// or module, so bare expressions would be rejected before Nix reaches
			// their syntax.
			expression = "let inputs = {}; pkgs = {}; in (\n" + expression + "\n)"
			cmd := exec.Command(nixInstantiate, "--parse", "--expr", expression)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("Nix example does not parse: %v\n%s", err, out)
			}
		})
	}
}

func nixBlockIsExpression(block string) bool {
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return strings.HasPrefix(line, "{") || strings.HasPrefix(line, "let ") || strings.HasPrefix(line, "rec {")
	}
	return true
}

func TestDocumentedSproutCommandsMatchTheCLI(t *testing.T) {
	for _, block := range markdownBlocks(t) {
		if block.language != "console" && block.language != "bash" && block.language != "sh" {
			continue
		}
		for offset, line := range strings.Split(block.body, "\n") {
			command, ok := documentedSproutCommand(line, block.language)
			if !ok {
				continue
			}
			name := fmt.Sprintf("%s:%d", block.path, block.line+offset)
			t.Run(name, func(t *testing.T) {
				assertDocumentedCommandParses(t, command)
			})
		}
	}
}

func documentedSproutCommand(line, language string) ([]string, bool) {
	line = strings.TrimSpace(line)
	if language == "console" {
		if !strings.HasPrefix(line, "$ sprout") {
			return nil, false
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "$ "))
	} else if !strings.HasPrefix(line, "sprout") {
		return nil, false
	}

	for _, separator := range []string{" #", " |", " >", " &&", " ||"} {
		if before, _, found := strings.Cut(line, separator); found {
			line = before
		}
	}
	fields := strings.Fields(line)
	if len(fields) == 0 || fields[0] != "sprout" {
		return nil, false
	}
	args := fields[1:]
	if len(args) > 0 && strings.IndexFunc(args[0], unicode.IsUpper) >= 0 {
		return nil, false
	}
	return args, true
}

func assertDocumentedCommandParses(t *testing.T, args []string) {
	t.Helper()

	root := newRootCmd()
	// Cobra installs completion lazily during execution, and the docs checker
	// stops before handlers run.
	root.InitDefaultCompletionCmd()
	cmd, remaining, err := root.Find(args)
	if err != nil {
		t.Fatalf("sprout %s: %v", strings.Join(args, " "), err)
	}
	if err := cmd.ParseFlags(remaining); err != nil {
		t.Fatalf("sprout %s: %v", strings.Join(args, " "), err)
	}
	if cmd.Args != nil {
		if err := cmd.Args(cmd, cmd.Flags().Args()); err != nil {
			t.Fatalf("sprout %s: %v", strings.Join(args, " "), err)
		}
	}
}
