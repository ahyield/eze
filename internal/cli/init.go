package cli

import (
	"embed"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed templates
var templatesFS embed.FS

// Init scaffolds a site directory: a starter page, eze.json, and instructions that teach
// coding agents (Claude Code, Codex, Cursor...) how to use the eze SDK.
func Init(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	siteFlag := fs.String("site", "", "site name (defaults to the directory name)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return errors.New("usage: eze init [dir] [--site name]")
	}
	dir := "."
	if len(pos) == 1 {
		dir = pos[0]
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	name := *siteFlag
	if name == "" {
		name = slug(filepath.Base(abs))
	}
	if !siteNameRe.MatchString(name) {
		return fmt.Errorf("%q is not a valid site name; pass --site with lowercase letters, digits and hyphens", name)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}

	files := map[string]string{
		"index.html": "templates/index.html",
		"AGENTS.md":  "templates/AGENTS.md",
		"CLAUDE.md":  "templates/CLAUDE.md",
	}
	for dst, src := range files {
		path := filepath.Join(abs, dst)
		if _, err := os.Stat(path); err == nil {
			fmt.Printf("  skip    %s (already exists)\n", dst)
			continue
		}
		b, err := templatesFS.ReadFile(src)
		if err != nil {
			return err
		}
		content := strings.ReplaceAll(string(b), "{{SITE}}", name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
		fmt.Printf("  create  %s\n", dst)
	}
	if p, _ := readProject(abs); p == nil {
		if err := writeProject(abs, &project{Site: name}); err != nil {
			return err
		}
		fmt.Printf("  create  %s\n", projectFile)
	}

	fmt.Printf("\nReady. Open %s in your coding agent and describe what you want, then run:\n", dir)
	if dir != "." {
		fmt.Printf("  cd %s\n", dir)
	}
	fmt.Println("  eze deploy")
	return nil
}
