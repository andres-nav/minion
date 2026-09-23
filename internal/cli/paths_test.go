package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/andres-nav/minion/internal/config"
)

func TestRemoteDirFor(t *testing.T) {
	cases := []struct {
		tmplDir, hostDir, root, want string
	}{
		{"", "", "/x/myrepo", "minion/myrepo"},                      // default, {repo} expanded
		{"", "~/exp/{repo}", "/x/myrepo", "~/exp/myrepo"},           // host dir used
		{"slot/{repo}", "~/exp/{repo}", "/x/myrepo", "slot/myrepo"}, // template wins over host
	}
	for _, c := range cases {
		if got := remoteDirFor(c.tmplDir, c.hostDir, c.root); got != c.want {
			t.Errorf("remoteDirFor(%q,%q,%q) = %q, want %q", c.tmplDir, c.hostDir, c.root, got, c.want)
		}
	}
}

func TestFetchDest(t *testing.T) {
	root := "/proj/repo"
	if got := fetchDest(root, "", "id1", "ts1", "repo"); got != root {
		t.Errorf("empty tmpl = %q, want root %q", got, root)
	}
	// {id}/{ts}/{repo} expand; relative path is under root.
	if got := fetchDest(root, "results/{repo}/{id}/{ts}", "id1", "ts1", "repo"); got != filepath.Join(root, "results/repo/id1/ts1") {
		t.Errorf("relative expansion = %q", got)
	}
	// absolute path is used verbatim (after expansion).
	if got := fetchDest(root, "/tmp/out/{id}", "id1", "ts1", "repo"); got != "/tmp/out/id1" {
		t.Errorf("absolute = %q, want /tmp/out/id1", got)
	}
	// leading ~ expands to the local home.
	if home, err := os.UserHomeDir(); err == nil {
		if got := fetchDest(root, "~/out", "id1", "ts1", "repo"); got != filepath.Join(home, "out") {
			t.Errorf("~ expansion = %q, want %q", got, filepath.Join(home, "out"))
		}
	}
}

func TestResolveHost(t *testing.T) {
	cfg := &config.Config{Hosts: map[string]config.Host{
		"gpu":  {SSH: "gpu-alias", RemoteDir: "x"},
		"bare": {}, // no SSH → alias defaults to the name
	}}
	r := &config.Resolved{HostName: "gpu"}

	// resolved template host, mapped through the hosts table.
	if h, alias := resolveHost(cfg, r, ""); alias != "gpu-alias" || h.RemoteDir != "x" {
		t.Errorf("default = %q %+v, want gpu-alias", alias, h)
	}
	// explicit override wins; SSH empty → alias is the name.
	if _, alias := resolveHost(cfg, r, "bare"); alias != "bare" {
		t.Errorf("override bare alias = %q, want bare", alias)
	}
	// a name not in the table is a raw ssh alias.
	if h, alias := resolveHost(cfg, r, "raw-host"); alias != "raw-host" || h != (config.Host{}) {
		t.Errorf("raw alias = %q %+v, want raw-host / empty host", alias, h)
	}
	// nothing configured → empty alias.
	if _, alias := resolveHost(cfg, &config.Resolved{}, ""); alias != "" {
		t.Errorf("no host alias = %q, want empty", alias)
	}
}
