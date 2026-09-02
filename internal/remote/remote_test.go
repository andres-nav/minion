package remote

import (
	"os/exec"
	"strings"
	"testing"
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
		"cd ~/work || exit 1",
		`export PATH="$HOME/.toolbox/bin:$PATH"`,
		"teardown() { ( cleanup ); :; }",
		"trap ",
		"> ~/.minion/jobs/j1/status; exit 143'",
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

func TestCommandBuildersReferenceJobDir(t *testing.T) {
	jd := JobDir("abc")
	if jd != "~/.minion/jobs/abc" {
		t.Fatalf("JobDir = %q", jd)
	}
	for _, c := range []string{LaunchCommand(jd), AttachCommand(jd), KillCommand(jd)} {
		if !strings.Contains(c, jd) {
			t.Errorf("command missing job dir %q: %s", jd, c)
		}
	}
	if !strings.Contains(PsCommand(), ".minion/jobs") {
		t.Errorf("PsCommand missing jobs dir: %s", PsCommand())
	}
}
