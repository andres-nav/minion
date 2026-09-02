package sync

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWalkGitRespectsGitignore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	write(t, filepath.Join(root, "main.py"), "code")
	write(t, filepath.Join(root, ".gitignore"), "ignored/\n*.log\n")
	write(t, filepath.Join(root, "ignored", "big.bin"), "junk")
	write(t, filepath.Join(root, "run.log"), "noise")
	write(t, filepath.Join(root, "new.txt"), "untracked-but-not-ignored")

	m, err := WalkGit(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m["main.py"]; !ok {
		t.Error("main.py should be included")
	}
	if _, ok := m["new.txt"]; !ok {
		t.Error("untracked non-ignored new.txt should be included")
	}
	for _, bad := range []string{"ignored/big.bin", "run.log"} {
		if _, ok := m[bad]; ok {
			t.Errorf("%s should be gitignored", bad)
		}
	}
}

func TestMatch(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "results", "a.json"), "1")
	write(t, filepath.Join(root, "results", "sub", "b.json"), "2")
	write(t, filepath.Join(root, "other.txt"), "3")

	// a directory pattern recurses
	got, err := Match(root, []string{"results"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Match(dir) = %v, want 2 files", got)
	}

	// a glob matches files
	got, err = Match(root, []string{"*.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "other.txt" {
		t.Fatalf("Match(glob) = %v, want [other.txt]", got)
	}
}

func TestDeleted(t *testing.T) {
	local := Manifest{"keep": {}, "add": {}}
	remote := Manifest{"keep": {}, "stale": {}}
	got := Deleted(local, remote)
	if len(got) != 1 || got[0] != "stale" {
		t.Fatalf("Deleted = %v, want [stale]", got)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWalkExcludes(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "main.tex"), "doc")
	write(t, filepath.Join(root, "build", "out.pdf"), "pdf")
	write(t, filepath.Join(root, ".git", "config"), "git")
	write(t, filepath.Join(root, "sub", "a.txt"), "a")

	m, err := Walk(root, []string{"build"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m["main.tex"]; !ok {
		t.Error("main.tex should be present")
	}
	if _, ok := m["sub/a.txt"]; !ok {
		t.Error("sub/a.txt should be present")
	}
	if _, ok := m["build/out.pdf"]; ok {
		t.Error("build/ should be excluded")
	}
	for k := range m {
		if k == ".git/config" {
			t.Error(".git should always be excluded")
		}
	}
}

func TestChanged(t *testing.T) {
	local := Manifest{
		"same":    {Size: 1, Mtime: 100},
		"newer":   {Size: 2, Mtime: 200},
		"created": {Size: 3, Mtime: 300},
	}
	remote := Manifest{
		"same":  {Size: 1, Mtime: 100},
		"newer": {Size: 2, Mtime: 150}, // different mtime
	}
	got := Changed(local, remote)
	want := []string{"created", "newer"} // sorted
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Changed = %v, want %v", got, want)
	}
}

func TestSendReceiveRoundTrip(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	write(t, filepath.Join(src, "a.txt"), "hello")
	write(t, filepath.Join(src, "nested", "b.bin"), "\x00\x01\x02binary\xff") // binary-safe
	// make one file executable to check mode is carried
	exe := filepath.Join(src, "run.sh")
	write(t, exe, "#!/bin/sh\n")
	if err := os.Chmod(exe, 0o755); err != nil {
		t.Fatal(err)
	}

	paths := []string{"a.txt", "nested/b.bin", "run.sh"}

	var buf bytes.Buffer
	if err := Send(&buf, src, paths, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	n, err := Receive(&buf, dst)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if n != len(paths) {
		t.Fatalf("received %d, want %d", n, len(paths))
	}

	for _, p := range paths {
		s, _ := os.ReadFile(filepath.Join(src, filepath.FromSlash(p)))
		d, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(p)))
		if err != nil {
			t.Fatalf("missing %s: %v", p, err)
		}
		if !bytes.Equal(s, d) {
			t.Fatalf("%s content mismatch", p)
		}
	}
	fi, err := os.Stat(filepath.Join(dst, "run.sh"))
	if err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("run.sh not executable on dest: %v", fi.Mode())
	}

	// after a round trip, manifests match → nothing changed.
	lm, _ := Walk(src, nil)
	rm, _ := Walk(dst, nil)
	if c := Changed(lm, rm); len(c) != 0 {
		t.Fatalf("expected no changes after sync, got %v", c)
	}
}
