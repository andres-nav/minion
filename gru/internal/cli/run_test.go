package cli

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andres-nav/minion/gru/internal/config"
	"github.com/andres-nav/minion/gru/internal/ssh"
)

func TestLaunchDrawsAnotherIDWhenTaken(t *testing.T) {
	state := t.TempDir()
	const ts = "20261123T120000Z"
	for _, id := range []string{"a-" + ts, "b-" + ts} {
		os.MkdirAll(filepath.Join(state, "jobs", id), 0o700)
	}
	names := []string{"a", "b", "c"}
	defer func(f func() string) { randomName = f }(randomName)
	randomName = func() string { n := names[0]; names = append(names[1:], n); return n }

	tg := &target{Client: ssh.Local(), state: state}
	id, err := launch(tg, ts, func(string) string { return "echo 0 > \"$(dirname \"$0\")/status\"\n" })
	if err != nil || id != "c-"+ts {
		t.Fatalf("id %q err %v", id, err)
	}
	os.MkdirAll(filepath.Join(state, "jobs", "c-"+ts), 0o700)
	if _, err := launch(tg, ts, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "no free job id") {
		t.Errorf("every id taken: err %v", err)
	}
}

func TestParsePositional(t *testing.T) {
	for args, want := range map[string]string{"x": "x", "x --host h": "x", "--host h x": "x", "": ""} {
		fs := flagSet()
		if got, err := parsePositional(fs, strings.Fields(args)); err != nil || got != want {
			t.Errorf("%q: got %q err %v", args, got, err)
		}
	}
	fs := flagSet()
	fs.SetOutput(new(strings.Builder))
	if _, err := parsePositional(fs, []string{"x", "--host", "h", "y"}); err == nil {
		t.Error("a second positional was accepted")
	}
}

// dial picks the --host override, else the template's host, through the hosts
// table (a name not in it is an ssh alias), and refuses no host at all.
func TestDial(t *testing.T) {
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nexit 255\n"), 0o755) // no real ssh
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	r := &config.Resolved{Root: "/x/proj", Template: config.Template{Host: "gpu"}, Config: &config.Config{Hosts: map[string]config.Host{
		"gpu":   {SSH: "gpu-alias", RemoteDir: "~/exp/{repo}", StateDir: "/fast/gru"},
		"local": {SSH: "localhost", StateDir: "/tmp/s"},
	}}}
	for override, want := range map[string]target{
		"":         {alias: "gpu-alias", dir: "~/exp/proj", state: "/fast/gru"},
		"raw-host": {alias: "raw-host", dir: "gru/proj", state: "~/.gru"},
		"local":    {alias: "localhost", dir: "/x/proj", state: "/tmp/s"},
	} {
		tg, err := dial(r, override, false)
		if err != nil || tg.alias != want.alias || tg.dir != want.dir || tg.state != want.state || tg.Local != (want.alias == "localhost") {
			t.Errorf("--host %q: %+v err %v, want %+v", override, tg, err, want)
		}
	}
	exe, _ := os.Executable()
	if tg, _ := dial(r, "localhost", false); tg.agent != "'"+exe+"'" {
		t.Errorf("local agent = %q, want this executable", tg.agent)
	}
	r.Template.Host = ""
	if _, err := dial(r, "", false); err != errNoHost {
		t.Errorf("no host: err %v", err)
	}
}

func flagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	hostFlag(fs)
	return fs
}

// The latest job is this project's newest by start time, whatever the dirs' mtimes.
func TestJobsNewestFirst(t *testing.T) {
	state := t.TempDir()
	host, _ := os.Hostname()
	for id, repo := range map[string]string{"a-20261123T100000Z": "proj", "b-20261123T110000Z": "other", "c-20261123T090000Z": "proj"} {
		d := filepath.Join(state, "jobs", id)
		os.MkdirAll(d, 0o700)
		os.WriteFile(filepath.Join(d, "hostname"), []byte(host+"\n"), 0o600)
		os.WriteFile(filepath.Join(d, "repo"), []byte(repo), 0o600)
		os.WriteFile(filepath.Join(d, "task"), []byte("t"), 0o600)
		os.WriteFile(filepath.Join(d, "status"), []byte("0\n"), 0o600)
	}
	tg := &target{Client: ssh.Local(), state: state, r: &config.Resolved{Root: "/x/proj"}}
	ids := func(all bool) (out []string) {
		js, err := jobs(tg, all)
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range js {
			out = append(out, j.id+" "+j.status)
		}
		return out
	}
	if got := strings.Join(ids(false), ","); got != "a-20261123T100000Z exit 0,c-20261123T090000Z exit 0" {
		t.Errorf("jobs = %s", got)
	}
	if got := len(ids(true)); got != 3 {
		t.Errorf("all jobs = %d", got)
	}
	if id, err := jobID(tg, ""); id != "a-20261123T100000Z" || err != nil {
		t.Errorf("latest = %q %v", id, err)
	}
	if _, err := jobID(tg, "../x"); err == nil {
		t.Error("a bad id was accepted")
	}
}
