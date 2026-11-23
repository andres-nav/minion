package cli

import (
	"flag"
	"fmt"
	"os"

	"github.com/andres-nav/minion/gru/internal/remote"
)

func cmdKill(args []string) int {
	fs := flag.NewFlagSet("kill", flag.ContinueOnError)
	host := hostFlag(fs)
	id, err := parsePositional(fs, args)
	if err != nil {
		return 2
	}
	if id == "" {
		fmt.Fprintln(os.Stderr, "usage: gru kill <id> [--host <name>]")
		return 2
	}
	if err := remote.CheckID(id); err != nil {
		return errf("%v", err)
	}
	r, err := project()
	if err != nil {
		return errf("%v", err)
	}
	t, err := dial(r, *host, false)
	if err != nil {
		return errf("%v", err)
	}
	code, err := t.Run(remote.KillCommand(remote.JobDir(t.state, id)), nil, os.Stdout, os.Stderr)
	if err != nil {
		return errf("%v", err)
	}
	return code
}
