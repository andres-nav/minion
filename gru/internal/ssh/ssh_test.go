package ssh

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveAgentBinary(t *testing.T) {
	// same platform → the running executable
	if got, err := ResolveAgentBinary("/usr/local/bin/gru", "darwin/arm64", "darwin/arm64", func(string) bool { return false }); err != nil || got != "/usr/local/bin/gru" {
		t.Fatalf("self: got %q err %v", got, err)
	}
	// cross platform, sibling present
	want := filepath.Join("/opt/bin", "gru-linux-amd64")
	if got, err := ResolveAgentBinary("/opt/bin/gru", "darwin/arm64", "linux/amd64", func(p string) bool { return p == want }); err != nil || got != want {
		t.Fatalf("cross-found: got %q err %v, want %q", got, err, want)
	}
	// cross platform, sibling missing → error
	if _, err := ResolveAgentBinary("/opt/bin/gru", "darwin/arm64", "linux/amd64", func(string) bool { return false }); err == nil {
		t.Fatal("cross-missing: expected error")
	}
	// cross platform, only the installed libexec copy present (bin/../libexec/gru)
	libexec := filepath.Join("/opt/bin", "..", "libexec", "gru", "gru-linux-arm64")
	if got, err := ResolveAgentBinary("/opt/bin/gru", "darwin/amd64", "linux/arm64", func(p string) bool { return p == libexec }); err != nil || got != libexec {
		t.Fatalf("libexec fallback: got %q err %v, want %q", got, err, libexec)
	}
	// malformed target platform → error
	if _, err := ResolveAgentBinary("/opt/bin/gru", "darwin/arm64", "bogus", func(string) bool { return true }); err == nil {
		t.Fatal("malformed platform: expected error")
	}
}

func TestSplitPlatform(t *testing.T) {
	if goos, goarch, ok := splitPlatform("linux/amd64"); !ok || goos != "linux" || goarch != "amd64" {
		t.Fatalf("got %q %q %v", goos, goarch, ok)
	}
	if _, _, ok := splitPlatform("bogus"); ok {
		t.Fatal("expected not ok for bogus input")
	}
}

func TestUnameToGo(t *testing.T) {
	cases := map[string]string{
		"Darwin arm64":    "darwin/arm64",
		"Linux x86_64":    "linux/amd64",
		"Linux aarch64":   "linux/arm64",
		"Linux amd64":     "linux/amd64",     // already-Go arch passes through
		"FreeBSD riscv64": "freebsd/riscv64", // unknown os+arch lower-cased/passed through
	}
	for in, want := range cases {
		if got := unameToGo(in); got != want {
			t.Errorf("unameToGo(%q) = %q, want %q", in, got, want)
		}
	}
}

// localClient returns a Client whose "ssh" runs the command locally under a
// temp HOME, emulating a same-arch remote host.
func localClient(home string) *Client {
	return &Client{
		Host: "fake",
		ssh: func(command string) *exec.Cmd {
			cmd := exec.Command("sh", "-c", command)
			cmd.Env = append(os.Environ(), "HOME="+home)
			return cmd
		},
	}
}

// fakeAgent writes a script that answers `_agent version` with version.
func fakeAgent(t *testing.T, version, extra string) string {
	p := filepath.Join(t.TempDir(), "gru-fake")
	script := "#!/bin/sh\n# " + extra + "\nif [ \"$1\" = \"_agent\" ] && [ \"$2\" = \"version\" ]; then echo \"gru " + version + "\"; fi\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEnsureAgentUploadsAndVerifies(t *testing.T) {
	home := t.TempDir()
	c := localClient(home)
	platform, err := c.RemotePlatform()
	if err != nil {
		t.Fatal(err)
	}
	name := "gru-testver-" + strings.ReplaceAll(platform, "/", "-")

	fake := fakeAgent(t, "testver", "")
	binaryFor := func(string) (string, error) { return fake, nil }
	invoke, err := c.EnsureAgent("~/state", "testver", binaryFor)
	if err != nil {
		t.Fatalf("EnsureAgent: %v", err)
	}
	if want := `"$HOME"/'state'/bin/'` + name + `'`; invoke != want {
		t.Fatalf("invoke = %q, want %q", invoke, want)
	}
	if fi, err := os.Stat(filepath.Join(home, "state")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("state dir not private: %v %v", fi.Mode(), err)
	}

	got := filepath.Join(home, "state/bin", name)
	fi, err := os.Stat(got)
	if err != nil {
		t.Fatalf("uploaded binary missing: %v", err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("uploaded binary not executable: %v", fi.Mode())
	}

	// second call hits the fast path (present, matching version).
	if _, err := c.EnsureAgent("~/state", "testver", binaryFor); err != nil {
		t.Fatalf("EnsureAgent (cached): %v", err)
	}
}

// A re-upload (dev builds always re-upload) replaces the file by a rename: an
// agent that has the old file open, or runs it, keeps the old bytes.
func TestEnsureAgentNeverRewritesInPlace(t *testing.T) {
	home := t.TempDir()
	c := localClient(home)
	first := fakeAgent(t, "dev", "first")
	if _, err := c.EnsureAgent("~/state", "dev", func(string) (string, error) { return first, nil }); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(home, "state/bin/gru-dev-*"))
	if len(matches) != 1 {
		t.Fatalf("agents: %v", matches)
	}
	running, err := os.Open(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	defer running.Close()

	second := fakeAgent(t, "dev", "second")
	if _, err := c.EnsureAgent("~/state", "dev", func(string) (string, error) { return second, nil }); err != nil {
		t.Fatal(err)
	}
	old, _ := io.ReadAll(running)
	now, _ := os.ReadFile(matches[0])
	if !strings.Contains(string(old), "first") || !strings.Contains(string(now), "second") {
		t.Fatalf("old file now %q, path now %q", old, now)
	}
	if left, _ := filepath.Glob(filepath.Join(home, "state/bin/.upload.*")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestOutputAndRun(t *testing.T) {
	c := localClient(t.TempDir())

	out, err := c.Output("echo hi")
	if err != nil || out != "hi" {
		t.Fatalf("Output = %q, err = %v", out, err)
	}

	code, err := c.Run("exit 5", nil, nil, nil)
	if err != nil {
		t.Fatalf("Run err = %v", err)
	}
	if code != 5 {
		t.Fatalf("Run exit = %d, want 5", code)
	}
}
