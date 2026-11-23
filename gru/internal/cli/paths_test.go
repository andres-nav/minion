package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
	_ "time/tzdata" // Europe/Zurich wherever the tests run
)

func TestRemoteDirFor(t *testing.T) {
	cases := []struct {
		tmplDir, hostDir, root, want string
	}{
		{"", "", "/x/myrepo", "gru/myrepo"},                         // default, {repo} expanded
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
	root, id := "/proj/repo", "brave_otter-20261123T142501Z"
	if got := fetchDest(root, "", id, "repo"); got != root {
		t.Errorf("empty tmpl = %q, want root %q", got, root)
	}
	// {id}/{ts}/{repo} expand; relative path is under root.
	if got := fetchDest(root, "results/{repo}/{ts}/{id}", id, "repo"); got != filepath.Join(root, "results/repo/20261123T142501Z", id) {
		t.Errorf("relative expansion = %q", got)
	}
	// absolute path is used verbatim (after expansion).
	if got := fetchDest(root, "/tmp/out/{id}", id, "repo"); got != "/tmp/out/"+id {
		t.Errorf("absolute = %q", got)
	}
	// leading ~ expands to the local home.
	if home, err := os.UserHomeDir(); err == nil {
		if got := fetchDest(root, "~/out", id, "repo"); got != filepath.Join(home, "out") {
			t.Errorf("~ expansion = %q, want %q", got, filepath.Join(home, "out"))
		}
	}
}

// Names and displays are UTC whatever the machine's zone (TZ=Europe/Zurich).
func TestTimesAreUTC(t *testing.T) {
	zurich, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TZ", "Europe/Zurich")
	local := time.Local
	time.Local = zurich // what TZ does at process start
	defer func() { time.Local = local }()

	at := time.Date(2026, 11, 23, 14, 25, 1, 0, time.UTC).In(zurich) // 15:25 in Zurich
	if got := stamp(at); got != "20261123T142501Z" {
		t.Errorf("stamp = %q", got)
	}
	start, ok := idTime("brave_otter-" + stamp(at))
	if got := psTime(start); !ok || got != "2026-11-23 14:25Z" {
		t.Errorf("ps time = %q", got)
	}
	if got := fetchDest("/r", "out/{ts}", "x-"+stamp(at), "r"); got != "/r/out/20261123T142501Z" {
		t.Errorf("fetch dest = %q", got)
	}
}
