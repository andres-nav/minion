package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/andres-nav/minion/internal/config"
)

type listOutput struct {
	Root      string        `json:"root"`
	GitRoot   string        `json:"git_root,omitempty"`
	Template  string        `json:"template"`
	Host      string        `json:"host"`
	RemoteDir string        `json:"remote_dir,omitempty"`
	RepoFile  string        `json:"repo_file,omitempty"`
	Tasks     []config.Task `json:"tasks"`
	Ambiguous []string      `json:"ambiguous,omitempty"`
}

func cmdList(args []string) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	_, r, err := resolveContext()
	if err != nil {
		return errf("%v", err)
	}

	tasks := r.Template.OrderedTasks()

	var remoteDir string
	if r.HostName != "" {
		remoteDir = remoteDirFor(r.Template.RemoteDir, r.Host.RemoteDir, r.Root)
	}

	if *jsonOut {
		out := listOutput{
			Root:      r.Root,
			GitRoot:   r.GitRoot,
			Template:  r.TemplateName,
			Host:      r.HostName,
			RemoteDir: remoteDir,
			RepoFile:  r.RepoFile,
			Tasks:     tasks,
			Ambiguous: r.Ambiguous,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return errf("%v", err)
		}
		return 0
	}

	if r.TemplateName == "" {
		fmt.Printf("no template matched in %s\n", r.Root)
		fmt.Println("(add detect rules to ~/.config/minion/config.json or a .minion.json with \"use\": \"...\")")
		return 0
	}

	fmt.Printf("template: %s\n", r.TemplateName)
	if r.HostName != "" {
		fmt.Printf("host:     %s\n", r.HostName)
		fmt.Printf("remote:   %s\n", remoteDir)
	}
	fmt.Printf("root:     %s\n", r.Root)
	if r.RepoFile != "" {
		fmt.Printf("config:   %s\n", r.RepoFile)
	}
	if len(r.Ambiguous) > 0 {
		fmt.Printf("note:     also matched %v (using %q)\n", r.Ambiguous, r.TemplateName)
	}
	if len(tasks) == 0 {
		fmt.Println("tasks:    (none defined)")
		return 0
	}
	fmt.Println("tasks:")
	for _, t := range tasks {
		fmt.Printf("  %-10s %s\n", t.Name, t.Command)
	}
	return 0
}
