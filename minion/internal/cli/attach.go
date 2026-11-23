package cli

import (
	"flag"
	"fmt"
	"os"

	"github.com/andres-nav/minion/minion/internal/remote"
)

func cmdAttach(args []string) int {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	host := hostFlag(fs)
	id, err := parsePositional(fs, args) // "" if none → most recent job below
	if err != nil {
		return 2
	}

	cfg, r, err := resolveContext()
	if err != nil {
		return errf("%v", err)
	}
	rt, err := dialRemote(cfg, r, *host) // the agent follows the log
	if err != nil {
		return errf("%v", err)
	}
	defer rt.close()

	if id == "" { // no id → most recent job
		latest, err := rt.exec.Output(remote.LatestJobCommand(rt.state))
		if err != nil {
			return errf("%v", err)
		}
		if latest == "" {
			return errf("no jobs on %s", rt.alias)
		}
		id = latest
		fmt.Fprintf(os.Stderr, "attaching latest: %s\n", id)
	}

	code, err := rt.exec.Run(remote.AttachCommand(notifyInvoke(rt), remote.JobDir(rt.state, id)), nil, os.Stdout, os.Stderr)
	if err != nil {
		return errf("%v", err)
	}
	return code
}
