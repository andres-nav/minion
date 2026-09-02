package cli

import (
	"flag"
	"fmt"
	"os"

	"github.com/andres-nav/minion/internal/remote"
	"github.com/andres-nav/minion/internal/ssh"
)

func cmdAttach(args []string) int {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	host := fs.String("host", "", "host")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	id := ""
	if fs.NArg() >= 1 {
		id = fs.Arg(0)
		if fs.NArg() > 1 { // allow flags after the id
			if err := fs.Parse(fs.Args()[1:]); err != nil {
				return 2
			}
		}
	}

	alias, err := aliasFor(*host)
	if err != nil {
		return errf("%v", err)
	}
	client := ssh.New(alias)

	if id == "" { // no id → most recent job
		latest, err := client.Output(remote.LatestJobCommand())
		if err != nil {
			return errf("%v", err)
		}
		if latest == "" {
			return errf("no jobs on %s", alias)
		}
		id = latest
		fmt.Fprintf(os.Stderr, "attaching latest: %s\n", id)
	}

	code, err := client.Run(remote.AttachCommand(remote.JobDir(id)), nil, os.Stdout, os.Stderr)
	if err != nil {
		return errf("%v", err)
	}
	return code
}
