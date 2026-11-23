package cli

import (
	"cmp"
	"errors"
	"fmt"
	"os"

	"github.com/andres-nav/minion/gru/internal/config"
	"github.com/andres-nav/minion/gru/internal/remote"
	"github.com/andres-nav/minion/gru/internal/ssh"
)

var errNoHost = errors.New("no host configured (set host in the template or .gru.json, or pass --host; use --host localhost to run on this machine)")

// errf prints a "gru: …" error to stderr and returns exit code 1.
func errf(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "gru: "+format+"\n", a...)
	return 1
}

// exit returns a command's exit code, or 1 after printing err.
func exit(code int, err error) int {
	if err != nil {
		return errf("%v", err)
	}
	return code
}

// project loads the config and resolves the project of the current directory.
// Its Config includes the project's .gru.json hosts and templates.
func project() (*config.Resolved, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return cfg.Resolve(cwd)
}

// connect resolves the project of the current directory and dials its host.
func connect(override string, agent bool) (*target, error) {
	r, err := project()
	if err != nil {
		return nil, err
	}
	return dial(r, override, agent)
}

// target is a host to run on: a client (ssh, or this machine for localhost),
// the project's dir there, the state dir that holds its jobs and agents, and
// the gru agent there as one shell word.
type target struct {
	*ssh.Client
	r     *config.Resolved
	host  config.Host
	alias string
	dir   string
	state string
	agent string
}

// dial resolves the host (override, else the template's; a name not in hosts
// is an ssh alias) and connects. A local target runs in the project itself
// with this executable as its agent; a remote one gets its agent bootstrapped
// when agent is set.
func dial(r *config.Resolved, override string, agent bool) (*target, error) {
	name := cmp.Or(override, r.Template.Host)
	if name == "" {
		return nil, errNoHost
	}
	h := r.Config.Hosts[name]
	t := &target{r: r, host: h, alias: cmp.Or(h.SSH, name), state: cmp.Or(h.StateDir, "~/.gru")}
	if t.alias == "localhost" || t.alias == "local" {
		exe, err := os.Executable()
		t.Client, t.dir, t.agent = ssh.Local(), r.Root, remote.ShellQuote(exe)
		return t, err
	}
	t.Client, t.dir = ssh.New(t.alias), remoteDirFor(r.Template.RemoteDir, h.RemoteDir, r.Root)
	if agent {
		t.Open() // one multiplexed connection for the whole command
		var err error
		if t.agent, err = t.EnsureAgent(t.state, version, ssh.AgentBinary); err != nil {
			t.Close()
			return nil, err
		}
	}
	return t, nil
}
