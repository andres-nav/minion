package cli

import (
	"flag"
	"fmt"
	"os"

	"github.com/andres-nav/minion/gru/internal/remote"
)

func cmdAttach(args []string) int {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	host := hostFlag(fs)
	id, err := parsePositional(fs, args)
	if err != nil {
		return 2
	}
	t, err := connect(*host, true) // the agent follows the log
	if err != nil {
		return errf("%v", err)
	}
	defer t.Close()
	latest := id == ""
	if id, err = jobID(t, id); err != nil {
		return errf("%v", err)
	}
	if id == "" {
		return errf("no jobs of this project on %s", t.alias)
	}
	if latest {
		fmt.Fprintf(os.Stderr, "attaching latest: %s\n", id)
	}
	return exit(t.Run(remote.AttachCommand(t.agent, remote.JobDir(t.state, id)), nil, os.Stdout, os.Stderr))
}
