package ssh

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAgentBinary(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "bin", "gru")
	if got, err := Binary("gru", runtime.GOOS+"/"+runtime.GOARCH); err != nil || got != must(os.Executable()) {
		t.Fatalf("own platform: got %q err %v", got, err)
	}
	if _, err := bundled(self, "gru-plan9-mips"); err == nil || !strings.Contains(err.Error(), "make build") {
		t.Fatalf("missing cross binary: err %v", err)
	}
	// next to the binary (the build tree), else in ../libexec/gru (installed).
	libexec := filepath.Join(dir, "libexec", "gru", "gru-plan9-mips")
	os.MkdirAll(filepath.Dir(libexec), 0o755)
	os.WriteFile(libexec, nil, 0o755)
	if got, err := bundled(self, "gru-plan9-mips"); err != nil || got != filepath.Join(dir, "bin", "..", "libexec", "gru", "gru-plan9-mips") {
		t.Fatalf("libexec: got %q err %v", got, err)
	}
	sibling := filepath.Join(dir, "bin", "gru-plan9-mips")
	os.MkdirAll(filepath.Dir(sibling), 0o755)
	os.WriteFile(sibling, nil, 0o755)
	if got, err := bundled(self, "gru-plan9-mips"); err != nil || got != sibling {
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
		if got := UnameToGo(in); got != want {
			t.Errorf("UnameToGo(%q) = %q, want %q", in, got, want)
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

// fakeAgent writes a script that answers `version` with version.
func fakeAgent(t *testing.T, version, extra string) string {
	p := filepath.Join(t.TempDir(), "gru-fake")
	script := "#!/bin/sh\n# " + extra + "\nif [ \"$1\" = \"version\" ]; then echo \"gru " + version + "\"; fi\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEnsureAgent(t *testing.T) {
	home := t.TempDir()
	c := localClient(home)
	unm, _ := c.Output("uname -sm")
	platform := UnameToGo(unm)
	ensure := func(bin string) string {
		t.Helper()
		invoke, err := c.EnsureAgent("~/state", "dev", platform, bin)
		if err != nil {
			t.Fatalf("EnsureAgent: %v", err)
		}
		return invoke
	}

	first := fakeAgent(t, "dev", "first")
	invoke := ensure(first)
	if !strings.HasPrefix(invoke, `"$HOME"/'state'/bin/'gru-dev-`+dashed(platform)+`-`) {
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
	if _, err := c.EnsureAgent("~/state", "v1", platform, fakeAgent(t, "v2", "")); err == nil {
		t.Fatal("a wrong agent version was accepted")
	}
}

func TestEnsureMinion(t *testing.T) {
	home := t.TempDir()
	c := localClient(home)
	bin := filepath.Join(t.TempDir(), "minion-linux-amd64")
	os.WriteFile(bin, []byte("build one"), 0o755)
	path, err := c.EnsureMinion("~/state", "v1", "linux/amd64", bin)
	// the first 12 hex digits of sha256("build one")
	if err != nil || path != `"$HOME"/'state'/bin/'minion-v1-linux-amd64-5fddb9545db6'` {
		t.Fatalf("EnsureMinion = %q, %v", path, err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, "state/bin/minion-v1-linux-amd64-5fddb9545db6")); string(b) != "build one" {
		t.Fatalf("uploaded %q", b)
	}
}

func TestUploadIsCheckedOnTheHost(t *testing.T) {
	home := t.TempDir()
	// A host whose cat changes what it writes: the upload never gets its name.
	c := &Client{Host: "fake", cmd: func(command string) *exec.Cmd {
		cmd := exec.Command("sh", "-c", strings.Replace(command, "cat > ", "{ cat; echo x; } > ", 1))
		cmd.Env = append(os.Environ(), "HOME="+home)
		return cmd
	}}
	bin := filepath.Join(t.TempDir(), "minion")
	os.WriteFile(bin, []byte("build"), 0o755)
	if _, err := c.EnsureMinion("~/state", "v1", "linux/amd64", bin); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("a changed upload: err %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(home, "state/bin/*")); len(left) != 0 {
		t.Fatalf("left %v", left)
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

// A user's ControlPersist makes an ssh master fork into the background once it
// is up. gru's own master must stay in front, so Close can end it.
func TestOwnMasterEndsWithClose(t *testing.T) {
	controlRoot = t.TempDir()
	defer func() { controlRoot = "/tmp" }()
	bin := t.TempDir()
	// Like OpenSSH with ControlPersist in ~/.ssh/config, after 0.3 s of login.
	fake := `#!/bin/sh
cp= persist=yes op=
while [ $# -gt 0 ]; do
  case $1 in
    -o) case $2 in ControlPath=*) cp=${2#ControlPath=} ;; ControlPersist=no) persist=no ;; esac; shift 2 ;;
    -O) op=$2; shift 2 ;;
    -*) shift ;;
    *) break ;;
  esac
done
case $op in
  check) [ -n "$cp" ] && [ -e "$cp" ]; exit ;;
  exit) kill "$(cat "$cp.pid")"; rm -f "$cp"; exit 0 ;;
esac
sleep 0.3
if [ $persist = yes ]; then sleep 30 & echo $! > "$cp.pid"; : > "$cp"; exit 0; fi
echo $$ > "$cp.pid"; : > "$cp"; exec sleep 30
`
	os.WriteFile(filepath.Join(bin, "ssh"), []byte(fake), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	c := New("fakehost")
	c.Open()
	b, err := os.ReadFile(c.ctlPath + ".pid")
	if err != nil {
		t.Fatalf("no master: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	c.Close()
	time.Sleep(100 * time.Millisecond)
	if syscall.Kill(pid, 0) == nil {
		t.Errorf("the master %d still runs after Close", pid)
	}
}

func must(s string, _ error) string { return s }
