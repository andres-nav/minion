package cli

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"runtime"

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
// the gru agent and minion there as shell words (minion "" if gru has none
// for the host's platform).
type target struct {
	*ssh.Client
	r      *config.Resolved
	host   config.Host
	name   string // what the user passes to --host, for hints
	alias  string
	dir    string
	state  string
	agent  string
	minion string
}

// aim resolves the host (override, else the template's; a name not in hosts
// is an ssh alias) without connecting. A local target runs in the project
// itself, with this executable as its agent.
func aim(r *config.Resolved, override string) (*target, error) {
	name := cmp.Or(override, r.Template.Host)
	if name == "" {
		return nil, errNoHost
	}
	h := r.Config.Hosts[name]
	t := &target{r: r, host: h, name: name, alias: cmp.Or(h.SSH, name), state: cmp.Or(h.StateDir, "~/.gru")}
	if t.alias == "localhost" || t.alias == "local" {
		exe, err := os.Executable()
		t.Client, t.dir, t.agent = ssh.Local(), r.Root, remote.ShellQuote(exe)
		return t, err
	}
	t.dir = remoteDirFor(r.Template.RemoteDir, h.RemoteDir, r.Root)
	return t, nil
}

// dial connects to the target aim resolves. With agent set, a remote host
// gets gru's agent, and any host minion's build for it, in its state dir.
func dial(r *config.Resolved, override string, agent bool) (*target, error) {
	t, err := aim(r, override)
	if err != nil {
		return nil, err
	}
	if t.Client == nil {
		t.Client = ssh.New(t.alias)
	}
	if agent {
		t.Open() // one multiplexed connection for the whole command
		if err := t.bootstrap(); err != nil {
			t.Close()
			return nil, err
		}
	}
	return t, nil
}

// bootstrap puts the gru agent on a remote host and, if gru carries a minion
// for the host's platform, minion on any host: named by its hash, so it finds
// its root copy by its own name.
func (t *target) bootstrap() error {
	platform := runtime.GOOS + "/" + runtime.GOARCH
	if !t.Local {
		unm, err := t.Output("uname -sm")
		if err != nil {
			return err
		}
		platform = ssh.UnameToGo(unm)
		bin, err := ssh.Binary("gru", platform)
		if err != nil {
			return err
		}
		if t.agent, err = t.EnsureAgent(t.state, version, platform, bin); err != nil {
			return err
		}
	}
	bin, err := ssh.Binary("minion", platform)
	if err != nil {
		return nil // tasks there get an empty GRU_MINION
	}
	t.minion, err = t.EnsureMinion(t.state, version, platform, bin)
	return err
}
