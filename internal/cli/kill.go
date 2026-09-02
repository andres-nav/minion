package cli

import (
	"flag"
	"fmt"
	"os"

	"github.com/andres-nav/minion/internal/remote"
)

func cmdKill(args []string) int {
	fs := flag.NewFlagSet("kill", flag.ContinueOnError)
	host := fs.String("host", "", "host")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: minion kill <id> [--host <name>]")
		return 2
	}
	id := fs.Arg(0)
	if fs.NArg() > 1 { // allow flags after the id
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return 2
		}
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
