package cli

import (
	"flag"
	"fmt"
	"os"

	"github.com/andres-nav/minion/internal/ssh"
)

// cmdSSH opens an interactive shell on the host in the project's remote dir.
// It does not bootstrap the agent (sync/run/fetch do that; doctor verifies it).
func cmdSSH(args []string) int {
	fs := flag.NewFlagSet("ssh", flag.ContinueOnError)
	host := fs.String("host", "", "host (a hosts-map name or ssh alias)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, r, err := resolveContext()
	if err != nil {
		return errf("%v", err)
	}
	h, alias := resolveHost(cfg, r, *host)
	if alias == "" {
		return errf("%v", errNoHost)
	}
	dir := remoteDirFor(r.Template.RemoteDir, h.RemoteDir, r.Root)

	fmt.Fprintf(os.Stderr, "%s: opening shell in %s\n", alias, dir)
	code, err := ssh.New(alias).Interactive(fmt.Sprintf("cd %s 2>/dev/null; exec ${SHELL:-sh} -l", dir))
	if err != nil {
		return errf("%v", err)
	}
	return code
}
