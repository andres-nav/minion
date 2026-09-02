package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/andres-nav/minion/internal/config"
	"github.com/andres-nav/minion/internal/ssh"
)

var errNoHost = errors.New("no host configured (set host in the template or .minion.json, or pass --host)")

// errf prints a "minion: …" error to stderr and returns exit code 1.
func errf(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "minion: "+format+"\n", a...)
	return 1
}

// resolveContext loads the global config and resolves the repo for cwd.
func resolveContext() (*config.Config, *config.Resolved, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, err
	}
	r, err := cfg.Resolve(cwd)
	if err != nil {
		return nil, nil, err
	}
	return cfg, r, nil
}

// resolveHost picks the ssh alias: an explicit override wins, else the resolved
// template/repo host mapped through the hosts table. A name not in the table is
// treated as a raw ssh alias. Returns an empty alias when none is configured.
func resolveHost(cfg *config.Config, r *config.Resolved, override string) (config.Host, string) {
	name := r.HostName
	if override != "" {
		name = override
	}
	if name == "" {
		return config.Host{}, ""
	}
	if h, ok := cfg.Hosts[name]; ok {
		alias := h.SSH
		if alias == "" {
			alias = name
		}
		return h, alias
	}
	return config.Host{}, name
}

// aliasFor resolves just the ssh alias for a command that doesn't bootstrap the
// agent (attach/kill/ps).
func aliasFor(override string) (string, error) {
	cfg, r, err := resolveContext()
	if err != nil {
		return "", err
	}
	if _, alias := resolveHost(cfg, r, override); alias != "" {
		return alias, nil
	}
	return "", errNoHost
}

// remoteDirFor computes the remote destination directory, expanding {repo}. A
// template's remote_dir (e.g. a workspace slot) takes precedence over the host's.
func remoteDirFor(tmplDir, hostDir, root string) string {
	rd := tmplDir
	if rd == "" {
		rd = hostDir
	}
	if rd == "" {
		rd = "minion/{repo}"
	}
	return strings.ReplaceAll(rd, "{repo}", filepath.Base(root))
}

// remoteTarget bundles a bootstrapped ssh client with its resolved context.
type remoteTarget struct {
	client   *ssh.Client
	invoke   string
	alias    string
	dir      string // remote destination directory
	resolved *config.Resolved
}

// dialRemote resolves the host, bootstraps the agent, and returns a ready
// target. It returns errNoHost when no host is configured.
func dialRemote(cfg *config.Config, r *config.Resolved, override string) (*remoteTarget, error) {
	host, alias := resolveHost(cfg, r, override)
	if alias == "" {
		return nil, errNoHost
	}
	client := ssh.New(alias)
	invoke, err := client.EnsureAgent(version, agentBinaryFor)
	if err != nil {
		return nil, err
	}
	return &remoteTarget{
		client:   client,
		invoke:   invoke,
		alias:    alias,
		dir:      remoteDirFor(r.Template.RemoteDir, host.RemoteDir, r.Root),
		resolved: r,
	}, nil
}
