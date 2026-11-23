package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andres-nav/minion/minion/internal/config"
	"github.com/andres-nav/minion/minion/internal/remote"
)

func TestResolveSecret(t *testing.T) {
	t.Setenv("SMTP_PW", "s3cret")
	cases := map[string]string{
		"${SMTP_PW}": "s3cret",
		"$SMTP_PW":   "s3cret",
		"literal":    "literal",
		"${MISSING}": "",
		"":           "",
	}
	for in, want := range cases {
		if got := resolveSecret(in); got != want {
			t.Errorf("resolveSecret(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolvedNotify(t *testing.T) {
	t.Setenv("PW", "app-password")
	t.Setenv("TOKEN", "s3cret-token")
	n := &config.Notify{
		Email: &config.EmailSink{SMTP: "smtp.x:587", From: "me@x", To: "me@x", Pass: "${PW}"},
		Run:   []string{"echo done"},
		Env:   map[string]string{"TOKEN": "${TOKEN}"},
	}
	s := resolvedNotify(n)
	for _, want := range []string{`"pass":"app-password"`, `"TOKEN":"s3cret-token"`, `"run":["echo done"]`} {
		if !strings.Contains(s, want) {
			t.Errorf("resolved block missing %s: %s", want, s)
		}
	}
	if strings.Contains(s, "${") || strings.Contains(s, "\n") || n.Email.Pass != "${PW}" {
		t.Errorf("secret reference left, not one line, or config mutated: %s", s)
	}
}

// The agent runs the hooks with the summary and the block's env, gated on `on`.
func TestAgentNotifyRunsHooks(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	cfg := filepath.Join(dir, "notify.json")
	n := config.Notify{
		On:  "failure",
		Run: []string{`printf '%s|%s' "$MINION_SUBJECT" "$TOKEN" > ` + out},
		Env: map[string]string{"TOKEN": "t0k"},
	}
	b, _ := json.Marshal(n)
	os.WriteFile(cfg, b, 0o600)
	t.Setenv("MINION_TASK", "build")
	t.Setenv("MINION_HOST", "gpu")

	t.Setenv("MINION_EXIT", "0")
	if code := agentNotify([]string{"--config", cfg}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("on: failure fired for exit 0")
	}
	t.Setenv("MINION_EXIT", "1")
	if code := agentNotify([]string{"--config", cfg}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got, _ := os.ReadFile(out); !strings.HasPrefix(string(got), "minion: build on gpu FAILED") || !strings.HasSuffix(string(got), "|t0k") {
		t.Errorf("hook saw %q", got)
	}

	// --config - reads the block from stdin (doctor --notify).
	r, w, _ := os.Pipe()
	w.Write(b)
	w.Close()
	stdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = stdin }()
	os.Remove(out)
	if code := agentNotify([]string{"--config", "-"}); code != 0 {
		t.Fatalf("stdin: exit %d", code)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("stdin config: hook not run: %v", err)
	}
}

// The job script, with a notify block, must still be valid sh.
func TestNotifyScriptIsValidShell(t *testing.T) {
	job := remote.Job{Command: "make", Agent: "~/.minion/bin/minion", Notify: resolvedNotify(&config.Notify{Run: []string{"echo 'done'"}})}
	s := remote.Script("~/w", "~/.minion/jobs/j1", job, remote.Meta{ID: "j1"})
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(s)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sh -n rejected script: %v\n%s\n---\n%s", err, out, s)
	}
}

func TestNotifyInvokeLocal(t *testing.T) {
	rt := &remoteTarget{local: true}
	got := notifyInvoke(rt)
	exe, _ := os.Executable()
	if got != remote.ShellQuote(exe) {
		t.Errorf("local notifyInvoke = %q, want quoted executable %q", got, exe)
	}
	remoteRT := &remoteTarget{invoke: "~/.minion/bin/minion"}
	if got := notifyInvoke(remoteRT); got != "~/.minion/bin/minion" {
		t.Errorf("remote notifyInvoke = %q", got)
	}
}
