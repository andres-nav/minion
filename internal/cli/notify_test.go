package cli

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/andres-nav/minion/internal/config"
	"github.com/andres-nav/minion/internal/remote"
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

func TestGate(t *testing.T) {
	if gate("failure") != `[ "$MINION_EXIT" != 0 ]` {
		t.Errorf("failure gate = %q", gate("failure"))
	}
	if gate("success") != `[ "$MINION_EXIT" = 0 ]` {
		t.Errorf("success gate = %q", gate("success"))
	}
	for _, on := range []string{"", "always", "weird"} {
		if gate(on) != "true" {
			t.Errorf("gate(%q) = %q, want true", on, gate(on))
		}
	}
}

func TestBuildNotify(t *testing.T) {
	t.Setenv("PW", "app-password")
	n := &config.Notify{
		On:    "failure",
		Email: &config.EmailSink{SMTP: "smtp.x:587", From: "me@x", To: "me@x", User: "me@x", Pass: "${PW}"},
		Run:   []string{"echo done"},
	}
	s := buildNotify("~/.minion/bin/minion", "~/.minion/jobs/j1/log", n)

	for _, want := range []string{
		"export MINION_SMTP_PASS='app-password'", // secret resolved locally, not left as ${PW}
		`if [ "$MINION_EXIT" != 0 ]; then`,       // gated on failure
		"_agent notify --smtp 'smtp.x:587' --from 'me@x' --to 'me@x' --user 'me@x' --log ~/.minion/jobs/j1/log",
		"( echo done )", // raw run hook included
	} {
		if !strings.Contains(s, want) {
			t.Errorf("snippet missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "${PW}") {
		t.Errorf("secret reference leaked into snippet:\n%s", s)
	}
}

func TestBuildNotifyRunOnly(t *testing.T) {
	n := &config.Notify{Run: []string{"curl example.com"}}
	s := buildNotify("minion", "~/log", n)
	if strings.Contains(s, "_agent notify") {
		t.Errorf("no email sink but agent invoked:\n%s", s)
	}
	if !strings.Contains(s, "( curl example.com )") {
		t.Errorf("run hook missing:\n%s", s)
	}
}

// The notify snippet, embedded in a job script, must still be valid sh.
func TestNotifyScriptIsValidShell(t *testing.T) {
	t.Setenv("PW", "pw")
	n := &config.Notify{
		Email: &config.EmailSink{SMTP: "smtp.x:587", From: "me@x", To: "a@x, b@x", Pass: "${PW}"},
		Run:   []string{"echo done"},
	}
	job := remote.Job{Command: "make", Notify: buildNotify("~/.minion/bin/minion", "~/.minion/jobs/j1/log", n)}
	s := remote.Script("~/w", "~/.minion/jobs/j1", job, remote.Meta{ID: "j1"})

	if !strings.Contains(s, "export MINION_JOB_ID='j1'") {
		t.Errorf("MINION_JOB_ID not exported:\n%s", s)
	}
	if !strings.Contains(s, `export MINION_STATUS`) {
		t.Errorf("MINION_STATUS not exported:\n%s", s)
	}
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
	if got != exe {
		t.Errorf("local notifyInvoke = %q, want executable %q", got, exe)
	}
	remoteRT := &remoteTarget{invoke: "~/.minion/bin/minion"}
	if got := notifyInvoke(remoteRT); got != "~/.minion/bin/minion" {
		t.Errorf("remote notifyInvoke = %q", got)
	}
}
