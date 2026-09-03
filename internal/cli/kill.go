package cli

import (
	"flag"
	"fmt"
	"os"

	"github.com/andres-nav/minion/internal/remote"
)

func cmdKill(args []string) int {
	fs := flag.NewFlagSet("kill", flag.ContinueOnError)
	host := hostFlag(fs)
	id, err := parsePositional(fs, args)
	if err != nil {
		return 2
	}
	if id == "" {
		fmt.Fprintln(os.Stderr, "usage: minion kill <id> [--host <name>]")
		return 2
	}

	client, _, err := executorFor(*host)
	if err != nil {
		return errf("%v", err)
	}
	code, err := client.Run(remote.KillCommand(remote.JobDir(id)), nil, os.Stdout, os.Stderr)
	if err != nil {
		return errf("%v", err)
	}
	return code
}
