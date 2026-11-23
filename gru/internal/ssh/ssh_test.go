package ssh

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAgentBinary(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "bin", "gru")
	if got, err := agentBinary(self, runtime.GOOS+"/"+runtime.GOARCH); err != nil || got != self {
		t.Fatalf("own platform: got %q err %v", got, err)
	}
	if _, err := agentBinary(self, "plan9/mips"); err == nil || !strings.Contains(err.Error(), "make cross") {
		t.Fatalf("missing cross binary: err %v", err)
	}
	// next to the binary (the build tree), else in ../libexec/gru (installed).
	libexec := filepath.Join(dir, "libexec", "gru", "gru-plan9-mips")
	os.MkdirAll(filepath.Dir(libexec), 0o755)
	os.WriteFile(libexec, nil, 0o755)
	if got, err := agentBinary(self, "plan9/mips"); err != nil || got != filepath.Join(dir, "bin", "..", "libexec", "gru", "gru-plan9-mips") {
		t.Fatalf("libexec: got %q err %v", got, err)
	}
	sibling := filepath.Join(dir, "bin", "gru-plan9-mips")
	os.MkdirAll(filepath.Dir(sibling), 0o755)
	os.WriteFile(sibling, nil, 0o755)
	if got, err := agentBinary(self, "plan9/mips"); err != nil || got != sibling {
		t.Fatalf("sibling: got %q err %v", got, err)
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

// localClient returns a Client that runs commands locally under a temp HOME,
// emulating a same-arch remote host.
func localClient(home string) *Client {
	return &Client{Host: "fake", cmd: func(command string) *exec.Cmd {
		cmd := exec.Command("sh", "-c", command)
		cmd.Env = append(os.Environ(), "HOME="+home)
		return cmd
	}}
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

func TestEnsureAgent(t *testing.T) {
	home := t.TempDir()
	c := localClient(home)
	unm, _ := c.Output("uname -sm")
	platform := strings.ReplaceAll(unameToGo(unm), "/", "-")
	ensure := func(bin string) string {
		t.Helper()
		invoke, err := c.EnsureAgent("~/state", "dev", func(string) (string, error) { return bin, nil })
		if err != nil {
			t.Fatalf("EnsureAgent: %v", err)
		}
		return invoke
	}

	first := fakeAgent(t, "dev", "first")
	invoke := ensure(first)
	if !strings.HasPrefix(invoke, `"$HOME"/'state'/bin/'gru-dev-`+platform+`-`) {
		t.Fatalf("invoke = %q", invoke)
	}
	if fi, err := os.Stat(filepath.Join(home, "state")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("state dir not private: %v %v", fi.Mode(), err)
	}
	agents, _ := filepath.Glob(filepath.Join(home, "state/bin/gru-dev-*"))
	if len(agents) != 1 {
		t.Fatalf("agents: %v", agents)
	}
	running, _ := os.Open(agents[0])
	defer running.Close()
	if ensure(first) != invoke {
		t.Fatal("the same build got another file")
	}

	// Another build of the same version gets its own file: a job running the
	// first one keeps calling the first one.
	second := fakeAgent(t, "dev", "second")
	if ensure(second) == invoke {
		t.Fatal("another build reused the file")
	}
	old, _ := io.ReadAll(running)
	if agents, _ = filepath.Glob(filepath.Join(home, "state/bin/gru-dev-*")); len(agents) != 2 || !strings.Contains(string(old), "first") {
		t.Fatalf("agents %v, first now %q", agents, old)
	}
	if left, _ := filepath.Glob(filepath.Join(home, "state/bin/.upload.*")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}

	// An agent that doesn't answer with the version is refused.
	if _, err := c.EnsureAgent("~/state", "v1", func(string) (string, error) { return fakeAgent(t, "v2", ""), nil }); err == nil {
		t.Fatal("a wrong agent version was accepted")
	}
}

func TestRunAndOutput(t *testing.T) {
	for _, c := range []*Client{localClient(t.TempDir()), Local()} {
		if out, err := c.Output("echo hi"); err != nil || out != "hi" {
			t.Fatalf("Output = %q, err = %v", out, err)
		}
		if _, err := c.Output("echo oops >&2; exit 3"); err == nil || !strings.Contains(err.Error(), "oops") || strings.Contains(err.Error(), "echo") {
			t.Fatalf("Output err = %v", err)
		}
		if code, err := c.Run("exit 5", nil, nil, nil); err != nil || code != 5 {
			t.Fatalf("Run = %d, %v; want 5", code, err)
		}
		var got string
		code, err := c.RunStdout("echo streamed", nil, func(r io.Reader) error { b, _ := io.ReadAll(r); got = string(b); return nil })
		if err != nil || code != 0 || got != "streamed\n" {
			t.Fatalf("RunStdout = %d, %v, %q", code, err, got)
		}
	}
}

func TestControlPathNeedsOwnPrivateDir(t *testing.T) {
	controlRoot = t.TempDir()
	defer func() { controlRoot = "/tmp" }()
	if p := controlPath("h"); p == "" || !strings.HasPrefix(p, controlRoot+"/gru-") {
		t.Fatalf("control path %q", p)
	}
	dir := filepath.Dir(controlPath("h"))
	os.Chmod(dir, 0o755)
	if p := controlPath("h"); p != "" {
		t.Fatalf("control path %q in a dir others can read", p)
	}
}
