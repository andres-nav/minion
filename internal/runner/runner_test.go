package runner

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func readTrace(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLocalSuccessOrder(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "trace")
	mark := func(s string) string { return "echo " + s + " >> " + out }

	code := Local(Job{
		Dir:       dir,
		Before:    []string{mark("before")},
		Command:   mark("cmd"),
		OnSuccess: []string{mark("success")},
		OnFailure: []string{mark("failure")},
		Always:    []string{mark("always")},
	}, &bytes.Buffer{}, &bytes.Buffer{})

	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got, want := readTrace(t, out), "before\ncmd\nsuccess\nalways\n"; got != want {
		t.Fatalf("trace = %q, want %q", got, want)
	}
}

func TestLocalFailureRunsOnFailure(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "trace")
	mark := func(s string) string { return "echo " + s + " >> " + out }

	code := Local(Job{
		Dir:       dir,
		Before:    []string{mark("before")},
		Command:   mark("cmd") + " && exit 3",
		OnSuccess: []string{mark("success")},
		OnFailure: []string{mark("failure")},
		Always:    []string{mark("always")},
	}, &bytes.Buffer{}, &bytes.Buffer{})

	if code != 3 {
		t.Fatalf("exit = %d, want 3", code)
	}
	if got, want := readTrace(t, out), "before\ncmd\nfailure\nalways\n"; got != want {
		t.Fatalf("trace = %q, want %q", got, want)
	}
}

func TestLocalBeforeFailureAborts(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "trace")
	mark := func(s string) string { return "echo " + s + " >> " + out }

	code := Local(Job{
		Dir:     dir,
		Before:  []string{mark("before") + " && exit 7"},
		Command: mark("cmd"),
		Always:  []string{mark("always")},
	}, &bytes.Buffer{}, &bytes.Buffer{})

	if code != 7 {
		t.Fatalf("exit = %d, want 7", code)
	}
	// command must be skipped; teardown still runs.
	if got, want := readTrace(t, out), "before\nalways\n"; got != want {
		t.Fatalf("trace = %q, want %q", got, want)
	}
}
