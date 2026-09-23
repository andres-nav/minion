package cli

import (
	"flag"
	"fmt"
	"os"
	"os/exec"

	"github.com/andres-nav/minion/internal/remote"
	"github.com/andres-nav/minion/internal/ssh"
)

// cmdSSH opens an interactive shell on the host in the project's remote dir (or
// locally for a localhost alias). It does not bootstrap the agent.
func cmdSSH(args []string) int {
	fs := flag.NewFlagSet("ssh", flag.ContinueOnError)
	host := hostFlag(fs)
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

	if isLocalAlias(alias) {
		fmt.Fprintf(os.Stderr, "local: opening shell in %s\n", r.Root)
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "sh"
		}
		cmd := exec.Command(shell, "-l")
		cmd.Dir = r.Root
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		code, cerr := ssh.ExitCode(cmd.Run())
		if cerr != nil {
			return errf("%v", cerr)
		}
		return code
	}

	dir := remoteDirFor(r.Template.RemoteDir, h.RemoteDir, r.Root)
	fmt.Fprintf(os.Stderr, "%s: opening shell in %s\n", alias, dir)
	code, err := ssh.New(alias).Interactive(fmt.Sprintf("cd %s 2>/dev/null; exec ${SHELL:-sh} -l", remote.RemotePath(dir)))
	if err != nil {
		return errf("%v", err)
	}
	return code
}
