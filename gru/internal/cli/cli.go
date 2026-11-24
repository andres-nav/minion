// Package cli implements gru's command dispatch and subcommands.
package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// version is overridden at build time via
// -ldflags "-X github.com/andres-nav/minion/gru/internal/cli.version=...".
var version = "dev"

type command struct {
	use     string
	summary string
	run     func(args []string) int
}

// NOTE: It's a function (not a var) to avoid an initialization cycle:
// cmdHelp → usage → commands.
func commands() []command {
	return []command{
		{"run <task>", "run a task of this project (--host, --detach, --arg, --no-sync)", cmdRun},
		{"list", "show the resolved template and tasks for this project (--json)", cmdList},
		{"sync", "copy modified files to the remote (--host, --delete)", cmdSync},
		{"fetch [<id>]", "pull result files back from the remote (--host, --into)", cmdFetch},
		{"attach [<id>]", "watch a job's output (default: the latest) (--host)", cmdAttach},
		{"kill <id>", "stop a running job (--host)", cmdKill},
		{"ps", "list this project's jobs (--all for every job) (--host)", cmdPs},
		{"ssh", "open an interactive shell on the host, in the project dir (--host)", cmdSSH},
		{"doctor", "check the config and hosts; --notify also sends a test notification (--host)", cmdDoctor},
		{"setup", "let minion do root's part on the host, with one sudo password (--host, --remove)", cmdSetup},
		{"minion", "run minion on the host: snapshot, restore, ... (--host)", cmdMinion},
		{"version", "print the gru version", cmdVersion},
		{"help", "show this help", cmdHelp},
		{"_agent", "", cmdAgent},
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
		if strings.Fields(c.use)[0] == name {
			return c.run(args[1:])
		}
	}

	fmt.Fprintf(os.Stderr, "gru: unknown command %q\n\n", args[0])
	usage(os.Stderr)
	return 2
}

func cmdVersion(args []string) int {
	fmt.Printf("gru %s\n", version)
	return 0
}

func cmdHelp(args []string) int {
	usage(os.Stdout)
	return 0
}

func usage(w io.Writer) {
	fmt.Fprint(w, `gru — a thin remote dev-loop orchestrator

usage: gru <command> [args]

commands:
`)
	for _, c := range commands() {
		if c.summary == "" {
			continue
		}
		fmt.Fprintf(w, "  %-13s %s\n", c.use, c.summary)
	}
}
