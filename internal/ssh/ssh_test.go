package ssh

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestResolveAgentBinary(t *testing.T) {
	// same platform → the running executable
	if got, err := ResolveAgentBinary("/usr/local/bin/minion", "darwin/arm64", "darwin/arm64", func(string) bool { return false }); err != nil || got != "/usr/local/bin/minion" {
		t.Fatalf("self: got %q err %v", got, err)
	}
	// cross platform, sibling present
	want := filepath.Join("/opt/bin", "minion-linux-amd64")
	if got, err := ResolveAgentBinary("/opt/bin/minion", "darwin/arm64", "linux/amd64", func(p string) bool { return p == want }); err != nil || got != want {
		t.Fatalf("cross-found: got %q err %v, want %q", got, err, want)
	}
	// cross platform, sibling missing → error
	if _, err := ResolveAgentBinary("/opt/bin/minion", "darwin/arm64", "linux/amd64", func(string) bool { return false }); err == nil {
		t.Fatal("cross-missing: expected error")
	}
	// cross platform, only the installed libexec copy present (bin/../libexec/minion)
	libexec := filepath.Join("/opt/bin", "..", "libexec", "minion", "minion-linux-arm64")
	if got, err := ResolveAgentBinary("/opt/bin/minion", "darwin/amd64", "linux/arm64", func(p string) bool { return p == libexec }); err != nil || got != libexec {
		t.Fatalf("libexec fallback: got %q err %v, want %q", got, err, libexec)
	}
	// malformed target platform → error
	if _, err := ResolveAgentBinary("/opt/bin/minion", "darwin/arm64", "bogus", func(string) bool { return true }); err == nil {
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

func TestEnsureAgentUploadsAndVerifies(t *testing.T) {
	home := t.TempDir()

	// a fake minion that answers `_agent version`.
	fake := filepath.Join(t.TempDir(), "minion-fake")
	script := "#!/bin/sh\nif [ \"$1\" = \"_agent\" ] && [ \"$2\" = \"version\" ]; then echo \"minion testver\"; fi\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	c := localClient(home)
	binaryFor := func(string) (string, error) { return fake, nil }
	invoke, err := c.EnsureAgent("testver", binaryFor)
	if err != nil {
		t.Fatalf("EnsureAgent: %v", err)
	}
	if want := "~/" + remoteBin; invoke != want {
		t.Fatalf("invoke = %q, want %q", invoke, want)
	}

	got := filepath.Join(home, remoteBin)
	fi, err := os.Stat(got)
	if err != nil {
		t.Fatalf("uploaded binary missing: %v", err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("uploaded binary not executable: %v", fi.Mode())
	}

	// second call hits the fast path (present, matching version).
	if _, err := c.EnsureAgent("testver", binaryFor); err != nil {
		t.Fatalf("EnsureAgent (cached): %v", err)
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
