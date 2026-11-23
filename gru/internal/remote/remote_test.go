package remote

import (
	"encoding/json"
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
		{Command: "make", OnFailure: []string{"a"}, Always: []string{"b"}, Notify: `{"run":["it's"]}`},
	}
	for _, j := range jobs {
		s := Script("~/w", "~/.gru/jobs/x", j)
		cmd := exec.Command("sh", "-n")
		cmd.Stdin = strings.NewReader(s)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sh -n rejected script: %v\n%s\n---\n%s", err, out, s)
		}
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
		"/s/$USER/x":   `'/s/'"${USER:?}"'/x'`, // variables expand on the remote, and must be set
	}
	for in, want := range cases {
		if got := RemotePath(in); got != want {
			t.Errorf("RemotePath(%q) = %q, want %q", in, got, want)
		}
	}
	out, code := sh(t, "unset NOPE; cd "+RemotePath("/tmp/$NOPE")+" && pwd", "")
	if code == 0 {
		t.Errorf("an unset variable resolved to %q", out)
	}
}

func TestCheckID(t *testing.T) {
	for _, id := range []string{"brave_otter-20261123T142501Z", "a"} {
		if err := CheckID(id); err != nil {
			t.Errorf("%q refused: %v", id, err)
		}
	}
	for _, id := range []string{"", "..", "../x", "a b", "x$(touch y)", "a/b"} {
		if err := CheckID(id); err == nil {
			t.Errorf("%q accepted", id)
		}
	}
}

func TestCommandBuildersReferenceJobDir(t *testing.T) {
	jd := JobDir("~/.gru", "abc")
	if jd != `"$HOME"/'.gru'/jobs/abc` {
		t.Fatalf("JobDir = %q", jd)
	}
	for _, c := range []string{LaunchCommand("~/.gru", "abc"), AttachCommand("gru", jd), KillCommand(jd)} {
		if !strings.Contains(c, jd) {
			t.Errorf("command missing job dir %q: %s", jd, c)
		}
	}
	if c := PsCommand("~/.gru"); !strings.Contains(c, JobsDir("~/.gru")) {
		t.Errorf("command missing jobs dir: %s", c)
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

// launch starts job j (with a fake agent) through LaunchCommand, as gru does,
// in work (default: a new dir), and returns the job dir and the agent's calls.
func launch(t *testing.T, j Job, work string) (jd, calls string) {
	t.Helper()
	dir := t.TempDir()
	state, calls := filepath.Join(dir, "state"), filepath.Join(dir, "calls")
	agent := filepath.Join(dir, "agent")
	fake := "#!/bin/sh\ncase $2 in now) echo 1700000000123456789;; notify) echo \"$GRU_STATUS $GRU_EXIT $*\" >> " + calls + ";; esac\n"
	os.WriteFile(agent, []byte(fake), 0o755)
	if work == "" {
		work = dir
	}
	j.ID, j.Agent = "j1", ShellQuote(agent)
	if _, code := sh(t, LaunchCommand(state, j.ID), Script(work, JobDir(state, j.ID), j)); code != 0 {
		t.Fatalf("launch exit %d", code)
	}
	return filepath.Join(state, "jobs", j.ID), calls
}

// wait waits for the job to record its status and its wrapper to exit.
func wait(t *testing.T, jd string) (status string, log string) {
	t.Helper()
	pid, _ := os.ReadFile(filepath.Join(jd, "pid"))
	p, _ := strconv.Atoi(strings.TrimSpace(string(pid)))
	for i := 0; i < 100 && syscall.Kill(p, 0) == nil; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	s, err := os.ReadFile(filepath.Join(jd, "status"))
	if err != nil {
		t.Fatalf("no status: %v", err)
	}
	l, _ := os.ReadFile(filepath.Join(jd, "log"))
	return strings.TrimSpace(string(s)), string(l)
}

// Every way a job ends runs the right hooks, records the status and run.json,
// and notifies.
func TestScriptEndings(t *testing.T) {
	hooks := Job{
		Notify:    `{"on":"always"}`,
		OnSuccess: []string{"echo success"},
		OnFailure: []string{"echo failure $GRU_EXIT"},
		Always:    []string{"echo always $GRU_STATUS"},
	}
	cases := []struct {
		name, command, status, log string
		before                     []string
		work                       string
	}{
		{"ok", "echo hi # a comment", "0", "hi\nsuccess\nalways ok\n", nil, ""},
		{"exit", "echo x; exit 7; echo y", "7", "x\nfailure 7\nalways fail\n", nil, ""},
		{"syntax", "echo (", "2", "failure 2\nalways fail\n", nil, ""},
		{"before", "echo never", "3", "b\nfailure 3\nalways fail\n", []string{"echo b; exit 3", "echo never"}, ""},
		{"cd", "echo never", "1", "failure 1\nalways fail\n", nil, "/nonexistent/dir"},
	}
	for _, c := range cases {
		j := hooks
		j.Command, j.Before = c.command, c.before
		jd, calls := launch(t, j, c.work)
		status, log := wait(t, jd)
		if status != c.status || !strings.HasSuffix(log, c.log) || strings.Contains(log, "never") {
			t.Errorf("%s: status %s log %q, want %s and %q", c.name, status, log, c.status, c.log)
		}
		var run struct{ Exit int }
		if b, err := os.ReadFile(filepath.Join(jd, "run.json")); err != nil || json.Unmarshal(b, &run) != nil || strconv.Itoa(run.Exit) != c.status {
			t.Errorf("%s: run.json %s %v", c.name, b, err)
		}
		if b, _ := os.ReadFile(calls); !strings.Contains(string(b), " "+c.status+" _agent notify") {
			t.Errorf("%s: agent calls %q", c.name, b)
		}
	}
}

func TestKilledJobRecordsAndNotifies(t *testing.T) {
	jd, calls := launch(t, Job{Command: "echo started; sleep 30", Always: []string{"echo always"}, Notify: `{"on":"failure"}`}, "")
	for i := 0; i < 100; i++ {
		if b, _ := os.ReadFile(filepath.Join(jd, "log")); strings.Contains(string(b), "started") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if out, code := sh(t, KillCommand(ShellQuote(jd)), ""); code != 0 || !strings.HasPrefix(out, "killed ") {
		t.Fatalf("kill: exit %d %q", code, out)
	}
	status, log := wait(t, jd)
	if status != "143" || !strings.HasSuffix(log, "[gru] killed\nalways\n") {
		t.Errorf("status %s log %q", status, log)
	}
	if b, _ := os.ReadFile(calls); !strings.HasPrefix(string(b), "fail 143 _agent notify") {
		t.Errorf("agent calls %q", b)
	}
	// A finished job is refused: its pid may belong to another process now.
	other := exec.Command("sleep", "30")
	other.Start()
	defer other.Process.Kill()
	os.WriteFile(filepath.Join(jd, "pid"), []byte(strconv.Itoa(other.Process.Pid)), 0o600)
	if _, code := sh(t, KillCommand(ShellQuote(jd)), ""); code != 1 {
		t.Errorf("kill of a finished job: exit %d, want 1", code)
	}
	var ws syscall.WaitStatus
	if pid, _ := syscall.Wait4(other.Process.Pid, &ws, syscall.WNOHANG, nil); pid != 0 {
		t.Errorf("kill signalled the pid of a finished job: %v", ws)
	}
	if b, _ := os.ReadFile(filepath.Join(jd, "log")); strings.Count(string(b), "killed") != 1 {
		t.Errorf("the finished job's log changed: %q", b)
	}
}

func TestScriptArgsAreLiteral(t *testing.T) {
	jd, _ := launch(t, Job{Command: `printf '%s\n' "$GRU_ARGS"`, Args: `fix it; rm -rf / $HOME '"`}, "")
	if _, log := wait(t, jd); log != "fix it; rm -rf / $HOME '\"\n" {
		t.Errorf("GRU_ARGS = %q", log)
	}
}

func TestJobsOfThisHostOnly(t *testing.T) {
	state := t.TempDir()
	script := "echo 0 > \"$(dirname \"$0\")/status\"\n"
	if _, code := sh(t, LaunchCommand(state, "mine"), script); code != 0 {
		t.Fatalf("launch exit %d", code)
	}
	// An id that is taken is refused, and the old job's files are kept.
	if _, code := sh(t, LaunchCommand(state, "mine"), "echo second\n"); code != IDTaken {
		t.Fatalf("relaunch exit %d, want %d", code, IDTaken)
	}
	mine := filepath.Join(state, "jobs", "mine")
	if b, _ := os.ReadFile(filepath.Join(mine, "run.sh")); string(b) != script {
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
	// Any other failure is not reported as a taken id.
	os.Chmod(filepath.Join(state, "jobs"), 0o500)
	if _, code := sh(t, LaunchCommand(state, "new"), ""); code == 0 || code == IDTaken {
		t.Errorf("launch into a read-only dir: exit %d", code)
	}
	os.Chmod(filepath.Join(state, "jobs"), 0o700)

	// A job of another host sharing the state dir, whose pid is alive here.
	other := filepath.Join(state, "jobs", "other")
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer sleeper.Process.Kill()
	os.Mkdir(other, 0o700)
	os.WriteFile(filepath.Join(other, "hostname"), []byte("elsewhere\n"), 0o600)
	os.WriteFile(filepath.Join(other, "pid"), []byte(strconv.Itoa(sleeper.Process.Pid)), 0o600)

	wait(t, mine)
	if out, _ := sh(t, PsCommand(state), ""); out != "mine\texit 0\t\t\n" {
		t.Errorf("ps = %q, want only mine", out)
	}
	if _, code := sh(t, KillCommand(JobDir(state, "other")), ""); code != 1 {
		t.Errorf("kill of another host's job exit %d, want 1", code)
	}
	var ws syscall.WaitStatus
	if pid, _ := syscall.Wait4(sleeper.Process.Pid, &ws, syscall.WNOHANG, nil); pid != 0 {
		t.Errorf("kill signalled another host's pid: %v", ws)
	}
	// A job of this host whose wrapper died without a status is lost.
	os.WriteFile(filepath.Join(other, "hostname"), []byte(host), 0o600)
	os.WriteFile(filepath.Join(other, "pid"), []byte("999999999"), 0o600)
	if out, _ := sh(t, PsCommand(state), ""); !strings.Contains(out, "other\tlost\t") {
		t.Errorf("ps = %q, want other lost", out)
	}
}

// A job records its provenance as valid JSON, gets GRU_* and the notify file,
// and hands the file to the agent at the end.
func TestScriptRecordsAndNotifies(t *testing.T) {
	block := `{"on":"always","run":["it's"]}`
	jd, calls := launch(t, Job{Task: `say "hi" \ 100%`, Host: "gpu", Repo: "proj", Git: "abc", Dirty: true,
		Notify: block, Command: `echo "$GRU_GIT $GRU_DIRTY $GRU_JOB_ID"; cat "$GRU_NOTIFY"`}, "")
	if status, log := wait(t, jd); status != "0" || log != "abc true j1\n"+block+"\n" {
		t.Fatalf("status %s, task saw %q", status, log)
	}
	if fi, err := os.Stat(filepath.Join(jd, "notify.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("notify.json not 0600: %v %v", fi.Mode(), err)
	}
	for f, want := range map[string]string{"task": `say "hi" \ 100%`, "repo": "proj"} {
		if b, _ := os.ReadFile(filepath.Join(jd, f)); string(b) != want {
			t.Errorf("%s = %q, want %q", f, b, want)
		}
	}
	var run struct {
		ID, Task, Host, Hostname, Kernel, Git string
		Dirty                                 bool
		StartNs                               int64 `json:"start_ns"`
		EndNs                                 int64 `json:"end_ns"`
		Exit                                  int
	}
	b, _ := os.ReadFile(filepath.Join(jd, "run.json"))
	if err := json.Unmarshal(b, &run); err != nil {
		t.Fatalf("run.json: %v: %s", err, b)
	}
	host, _ := sh(t, "uname -n", "")
	kernel, _ := sh(t, "uname -r", "")
	if run.ID != "j1" || run.Task != `say "hi" \ 100%` || run.Host != "gpu" || run.StartNs != 1700000000123456789 || run.EndNs != run.StartNs ||
		run.Hostname+"\n" != host || run.Kernel+"\n" != kernel || run.Git != "abc" || !run.Dirty || run.Exit != 0 {
		t.Errorf("run.json = %s", b)
	}
	want := "ok 0 _agent notify --config " + jd + "/notify.json --log " + jd + "/log\n"
	if got, _ := os.ReadFile(calls); string(got) != want {
		t.Errorf("agent called with %q, want %q", got, want)
	}
}
