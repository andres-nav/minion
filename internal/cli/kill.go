package cli

import (
	"flag"
	"fmt"
	"os"

	"github.com/andres-nav/minion/internal/remote"
	"github.com/andres-nav/minion/internal/ssh"
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

	alias, err := aliasFor(*host)
	if err != nil {
		fmt.Fprintf(os.Stderr, "minion: %v\n", err)
		return 1
	}
	code, err := ssh.New(alias).Run(remote.KillCommand(remote.JobDir(id)), nil, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "minion: %v\n", err)
		return 1
	}
	return code
}
