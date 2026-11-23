package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

func cmdList(args []string) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	r, err := project()
	if err != nil {
		return errf("%v", err)
	}
	tm := r.Template
	var remoteDir string
	if t, err := aim(r, ""); err == nil {
		remoteDir = t.dir
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]any{"root": r.Root, "template": r.TemplateName, "host": tm.Host, "remote_dir": remoteDir,
			"repo_file": r.RepoFile, "tasks": tm.Tasks, "ambiguous": r.Ambiguous})
		return 0
	}
	if len(tm.Tasks) == 0 {
		fmt.Printf("no tasks for %s\n", r.Root)
		fmt.Println("(add detect rules to ~/.config/gru/config.json, or a .gru.json with \"use\" or \"tasks\")")
		return 0
	}
	fmt.Printf("template: %s\n", r.TemplateName)
	if tm.Host != "" {
		fmt.Printf("host:     %s\nremote:   %s\n", tm.Host, remoteDir)
	}
	fmt.Printf("root:     %s\n", r.Root)
	if r.RepoFile != "" {
		fmt.Printf("config:   %s\n", r.RepoFile)
	}
	if len(r.Ambiguous) > 0 {
		fmt.Printf("note:     also matched %v (using %q)\n", r.Ambiguous, r.TemplateName)
	}
	fmt.Println("tasks:")
	for _, n := range tm.TaskNames() {
		fmt.Printf("  %-10s %s\n", n, tm.Tasks[n])
	}
	return 0
}
