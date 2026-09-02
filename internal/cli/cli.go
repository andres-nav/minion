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

// Run dispatches a subcommand and returns the process exit code.
func Run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}

	switch args[0] {
	case "version", "--version", "-v":
		fmt.Printf("minion %s\n", version)
		return 0
	case "help", "--help", "-h":
		usage(os.Stdout)
		return 0
	case "build", "test", "run":
		return cmdTask(args[0], args[1:])
	case "list":
		return cmdList(args[1:])
	case "sync":
		return cmdSync(args[1:])
	case "fetch":
		return cmdFetch(args[1:])
	case "attach":
		return cmdAttach(args[1:])
	case "kill":
		return cmdKill(args[1:])
	case "ps":
		return cmdPs(args[1:])
	case "ssh":
		return cmdSSH(args[1:])
	case "doctor":
		return cmdDoctor(args[1:])
	case "_agent":
		return cmdAgent(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "minion: unknown command %q\n\n", args[0])
		usage(os.Stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `minion — a thin remote dev-loop orchestrator

usage: minion <command> [args]

commands:
  build         run the project's build task
  test          run the test task
  run <task>    run any task by name (e.g. lint, clean, or a freeform task)
  list          show the resolved template and tasks for this repo
  sync          copy modified files to the remote (--host)
  fetch         pull result files back from the remote (--host, --into)
  attach [<id>] watch a remote job's output (default: the latest) (--host)
  kill <id>     stop a running remote job (--host)
  ps            list this package's remote jobs (--all for every job) (--host)
  ssh           open an interactive shell on the host, in the project dir (--host)
  doctor        validate config and check hosts (reachable + agent) (--host)
  version       print the minion version
  help          show this help
`)
}
