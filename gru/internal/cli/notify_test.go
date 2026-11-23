package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andres-nav/minion/gru/internal/config"
)

func TestResolveSecret(t *testing.T) {
	t.Setenv("SMTP_PW", "s3cret")
	for in, want := range map[string]string{"${SMTP_PW}": "s3cret", "$SMTP_PW": "s3cret", "literal": "literal", "": ""} {
		if got, err := resolveSecret(in); err != nil || got != want {
			t.Errorf("resolveSecret(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"${MISSING}", "$MISSING"} {
		if _, err := resolveSecret(in); err == nil || !strings.Contains(err.Error(), "$MISSING is not set") {
			t.Errorf("resolveSecret(%q): err %v", in, err)
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
	s, err := resolvedNotify(n)
	for _, want := range []string{`"pass":"app-password"`, `"TOKEN":"s3cret-token"`, `"run":["echo done"]`} {
		if err != nil || !strings.Contains(s, want) {
			t.Errorf("resolved block missing %s: %s %v", want, s, err)
		}
	}
	if strings.Contains(s, "${") || strings.Contains(s, "\n") || n.Email.Pass != "${PW}" {
		t.Errorf("secret reference left, not one line, or config mutated: %s", s)
	}
	n.Env["X"] = "$UNSET_VAR"
	if _, err := resolvedNotify(n); err == nil {
		t.Error("an unset secret was sent as empty")
	}
	if s, err := resolvedNotify(nil); s != "" || err != nil {
		t.Errorf("no block: %q %v", s, err)
	}
}

// The agent runs the hooks with the summary and the block's env, gated on `on`.
func TestAgentNotifyRunsHooks(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	cfg := filepath.Join(dir, "notify.json")
	n := config.Notify{
		On:  "failure",
		Run: []string{`printf '%s|%s|%s' "$GRU_SUBJECT" "$GRU_BODY" "$TOKEN" > ` + out},
		Env: map[string]string{"TOKEN": "t0k"},
	}
	b, _ := json.Marshal(n)
	os.WriteFile(cfg, b, 0o600)
	t.Setenv("GRU_TASK", "build")
	t.Setenv("GRU_HOST", "gpu")

	t.Setenv("GRU_EXIT", "0")
	if code := agentNotify([]string{"--config", cfg}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("on: failure fired for exit 0")
	}
	// A task's own message skips the gate and replaces the job summary.
	if code := agentNotify([]string{"--config", cfg, "--subject", "run_failed", "--body", "run 3"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got, _ := os.ReadFile(out); string(got) != "run_failed|run 3|t0k" {
		t.Errorf("hook saw %q", got)
	}
	t.Setenv("GRU_EXIT", "1")
	if code := agentNotify([]string{"--config", cfg}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got, _ := os.ReadFile(out); !strings.HasPrefix(string(got), "gru: build on gpu FAILED") || !strings.HasSuffix(string(got), "|t0k") {
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

// A notify that runs past its timeout is killed with everything it started.
func TestAgentNotifyTimesOut(t *testing.T) {
	dir := t.TempDir()
	cfg, mark := filepath.Join(dir, "notify.json"), filepath.Join(dir, "late")
	b, _ := json.Marshal(config.Notify{Run: []string{"(sleep 1; touch " + mark + ") & sleep 30"}})
	os.WriteFile(cfg, b, 0o600)
	defer func(d time.Duration) { notifyTimeout = d }(notifyTimeout)
	notifyTimeout = 200 * time.Millisecond
	begin := time.Now()
	if code := agentNotify([]string{"--config", cfg}); code != 1 || time.Since(begin) > 5*time.Second {
		t.Fatalf("exit %d after %v", code, time.Since(begin))
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(mark); err == nil {
		t.Error("the hook's child outlived the timeout")
	}
}

func TestAgentArgs(t *testing.T) {
	for _, args := range [][]string{{}, {"bogus"}, {"follow"}, {"send"}, {"notify"}} {
		if code := cmdAgent(args); code != 2 {
			t.Errorf("_agent %v: exit %d, want 2", args, code)
		}
	}
	if code := agentNotify([]string{"--config", filepath.Join(t.TempDir(), "none")}); code != 1 {
		t.Errorf("missing config file: exit %d", code)
	}
}
