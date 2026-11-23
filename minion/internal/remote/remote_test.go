package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestScriptIsValidShell(t *testing.T) {
	// empty hook lists must still yield syntactically valid sh.
	jobs := []Job{
		{Command: "make"},
		{Command: "make", Before: []string{"a"}},
		{Command: "make", OnSuccess: []string{"a"}},
		{Command: "make", OnFailure: []string{"a"}, Always: []string{"b"}},
	}
	for _, j := range jobs {
		s := Script("~/w", "~/.minion/jobs/x", j, Meta{})
		cmd := exec.Command("sh", "-n")
		cmd.Stdin = strings.NewReader(s)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sh -n rejected script: %v\n%s\n---\n%s", err, out, s)
		}
	}
}

func TestScript(t *testing.T) {
	j := Job{
		Command:   "make x",
		Env:       []string{"PATH=$HOME/.toolbox/bin:$PATH"},
		Before:    []string{"mwinit -f"},
		OnFailure: []string{"notify fail"},
		Always:    []string{"cleanup"},
	}
	s := Script("~/work", "~/.minion/jobs/j1", j, Meta{ID: "j1", Task: "build", Host: "gpu"})

	for _, want := range []string{
		`cd "$HOME"/'work' || exit 1`,
		`export PATH="$HOME/.toolbox/bin:$PATH"`,
		"teardown() { ( cleanup ); :; }",
		"echo 143 > ~/.minion/jobs/j1/status; exit 143; }",
		"trap killed TERM INT HUP",
		"( mwinit -f ) || fail $?",
		"( make x )\nec=$?",
		"( notify fail )",
		`echo "$ec" > ~/.minion/jobs/j1/status`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script missing %q\n---\n%s", want, s)
		}
	}
}

func TestScriptArgs(t *testing.T) {
	// task text is single-quoted, so shell metacharacters stay literal (no injection).
	s := Script("~/w", "~/.minion/jobs/x", Job{Command: `claude -p "$MINION_ARGS"`, Args: "fix it; rm -rf /"}, Meta{})
	if !strings.Contains(s, `export MINION_ARGS='fix it; rm -rf /'`) {
		t.Errorf("MINION_ARGS not safely exported:\n%s", s)
	}
	// no Args → no export line.
	if s := Script("~/w", "~/.minion/jobs/x", Job{Command: "make"}, Meta{}); strings.Contains(s, "MINION_ARGS") {
		t.Errorf("unexpected MINION_ARGS export with no args:\n%s", s)
	}
}

func TestRemotePath(t *testing.T) {
	cases := map[string]string{
		"~":            `"$HOME"`,
		"~/work":       `"$HOME"/'work'`,
		"~/a b/c":      `"$HOME"/'a b/c'`,      // spaces stay literal
		"/abs path":    `'/abs path'`,          // absolute quoted whole
		"rel/dir":      `'rel/dir'`,            // $HOME-relative quoted whole
		"has'quote":    `'has'\''quote'`,       // embedded single quote escaped
		"~/x;rm -rf .": `"$HOME"/'x;rm -rf .'`, // metacharacters neutralized
	}
	for in, want := range cases {
		if got := RemotePath(in); got != want {
			t.Errorf("RemotePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCommandBuildersReferenceJobDir(t *testing.T) {
	jd := JobDir("~/.minion", "abc")
	if jd != `"$HOME"/'.minion'/jobs/abc` {
		t.Fatalf("JobDir = %q", jd)
	}
	for _, c := range []string{LaunchCommand("~/.minion", "abc"), AttachCommand("minion", jd), KillCommand(jd)} {
		if !strings.Contains(c, jd) {
			t.Errorf("command missing job dir %q: %s", jd, c)
		}
	}
	for _, c := range []string{PsCommand("~/.minion"), LatestJobCommand("~/.minion")} {
		if !strings.Contains(c, JobsDir("~/.minion")) {
			t.Errorf("command missing jobs dir: %s", c)
		}
	}
}

// sh runs a command builder's output locally and returns its stdout and exit code.
func sh(t *testing.T, command, stdin string) (string, int) {
	t.Helper()
	cmd := exec.Command("sh", "-c", command)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), 0
}

func TestJobsOfThisHostOnly(t *testing.T) {
	state := t.TempDir()
	if _, code := sh(t, LaunchCommand(state, "mine"), "exit 0\n"); code != 0 {
		t.Fatalf("launch exit %d", code)
	}
	// An id that is taken is refused, and the old job's files are kept.
	if _, code := sh(t, LaunchCommand(state, "mine"), "echo second\n"); code != IDTaken {
		t.Fatalf("relaunch exit %d, want %d", code, IDTaken)
	}
	mine := filepath.Join(state, "jobs", "mine")
	if b, _ := os.ReadFile(filepath.Join(mine, "run.sh")); string(b) != "exit 0\n" {
		t.Fatalf("run.sh overwritten: %q", b)
	}
	for _, p := range []string{filepath.Join(state, "jobs"), mine} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s not private: %v %v", p, fi.Mode(), err)
		}
	}
	host, _ := sh(t, "uname -n", "")
	if b, _ := os.ReadFile(filepath.Join(mine, "hostname")); string(b) != host {
		t.Fatalf("hostname = %q, want %q", b, host)
	}

	// A newer job of another host sharing the state dir, whose pid is alive here.
	other := filepath.Join(state, "jobs", "other")
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer sleeper.Process.Kill()
	os.Mkdir(other, 0o700)
	os.WriteFile(filepath.Join(other, "hostname"), []byte("elsewhere\n"), 0o600)
	os.WriteFile(filepath.Join(other, "pid"), []byte(strconv.Itoa(sleeper.Process.Pid)), 0o600)
	future := time.Now().Add(time.Hour)
	os.Chtimes(other, future, future)

	if out, _ := sh(t, PsCommand(state), ""); !strings.HasPrefix(out, "mine\t") || strings.Contains(out, "other") {
		t.Errorf("ps = %q, want only mine", out)
	}
	if out, _ := sh(t, LatestJobCommand(state), ""); out != "mine\n" {
		t.Errorf("latest = %q, want mine", out)
	}
	if _, code := sh(t, KillCommand(JobDir(state, "other")), ""); code != 1 {
		t.Errorf("kill of another host's job exit %d, want 1", code)
	}
	var ws syscall.WaitStatus
	if pid, _ := syscall.Wait4(sleeper.Process.Pid, &ws, syscall.WNOHANG, nil); pid != 0 {
		t.Errorf("kill signalled another host's pid: %v", ws)
	}
	if _, err := os.Stat(filepath.Join(other, "status")); err == nil {
		t.Error("kill wrote another host's status")
	}
}
