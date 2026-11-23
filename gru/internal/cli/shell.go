package cli

import (
	"flag"
	"fmt"
	"os"

	"github.com/andres-nav/minion/gru/internal/remote"
)

// cmdSSH opens an interactive shell on the host in the project's remote dir.
func cmdSSH(args []string) int {
	fs := flag.NewFlagSet("ssh", flag.ContinueOnError)
	host := hostFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	t, err := connect(*host, false)
	if err != nil {
		return errf("%v", err)
	}
	if t.Local {
		return errf("%s is this machine", t.alias)
	}
	fmt.Fprintf(os.Stderr, "%s: opening shell in %s\n", t.alias, t.dir)
	return exit(t.Interactive(fmt.Sprintf("cd %s 2>/dev/null; exec ${SHELL:-sh} -l", remote.RemotePath(t.dir))))
}
