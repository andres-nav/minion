package cli

import (
	"flag"
	"fmt"
	"os"

	"github.com/andres-nav/minion/internal/remote"
)

func cmdAttach(args []string) int {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	host := hostFlag(fs)
	id, err := parsePositional(fs, args) // "" if none → most recent job below
	if err != nil {
		return 2
	}

	client, alias, err := executorFor(*host)
	if err != nil {
		return errf("%v", err)
	}

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
