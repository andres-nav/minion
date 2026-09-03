// Package cli implements minion's command dispatch and subcommands.
package cli

import (
	"fmt"
	"io"
	"os"
)

// version is overridden at build time via
// -ldflags "-X github.com/andres-nav/minion/internal/cli.version=...".
var version = "dev"

type command struct {
	name    string
	use     string
	summary string
	run     func(args []string) int
}

// NOTE: It's a function (not a var) to avoid an initialization cycle:
// cmdHelp → usage → commands.
func commands() []command {
	return []command{
		{"run", "run <task>", "run a task by name (build, test, lint, clean, or a freeform task)", cmdRun},
		{"list", "", "show the resolved template and tasks for this repo", cmdList},
		{"sync", "", "copy modified files to the remote (--host)", cmdSync},
		{"fetch", "", "pull result files back from the remote (--host, --into)", cmdFetch},
		{"attach", "attach [<id>]", "watch a remote job's output (default: the latest) (--host)", cmdAttach},
		{"kill", "kill <id>", "stop a running remote job (--host)", cmdKill},
		{"ps", "", "list this package's remote jobs (--all for every job) (--host)", cmdPs},
		{"ssh", "", "open an interactive shell on the host, in the project dir (--host)", cmdSSH},
		{"doctor", "", "validate config and check hosts; --notify also sends a test email (--host)", cmdDoctor},
		{"version", "", "print the minion version", cmdVersion},
		{"help", "", "show this help", cmdHelp},
		{"_agent", "", "", cmdAgent},
	}
}

func Run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}

	name := args[0]
	switch name {
	case "--version", "-v":
		name = "version"
	case "--help", "-h":
		name = "help"
	}

	for _, c := range commands() {
		if c.name == name {
			return c.run(args[1:])
		}
	}

	fmt.Fprintf(os.Stderr, "minion: unknown command %q\n\n", args[0])
	usage(os.Stderr)
	return 2
}

func cmdVersion(args []string) int {
	fmt.Printf("minion %s\n", version)
	return 0
}

func cmdHelp(args []string) int {
	usage(os.Stdout)
	return 0
}

func usage(w io.Writer) {
	fmt.Fprint(w, `minion — a thin remote dev-loop orchestrator

usage: minion <command> [args]

commands:
`)
	for _, c := range commands() {
		if c.summary == "" {
			continue
		}
		label := c.name
		if c.use != "" {
			label = c.use
		}
		fmt.Fprintf(w, "  %-13s %s\n", label, c.summary)
	}
}
