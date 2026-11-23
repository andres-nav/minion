package ssh

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/andres-nav/minion/gru/internal/remote"
)

// Client runs commands on a host via the system ssh binary, inheriting the
// user's ~/.ssh/config (ProxyJump, ControlMaster, ProxyCommand, …), or on this
// machine via `sh -c` (Local).
type Client struct {
	Host       string
	Local      bool
	ctlPath    string        // our control-master socket ("" when reusing the user's master, or none)
	opts       []string      // ssh options shared by every command
	master     *exec.Cmd     // the control-master process, if Open()ed
	masterDone chan struct{} // closed when the master process exits (its Wait is owned by Open)
	cmd        func(command string) *exec.Cmd
}

// Local returns a client that runs commands on this machine.
func Local() *Client {
	return &Client{Host: "localhost", Local: true, cmd: func(c string) *exec.Cmd { return exec.Command("sh", "-c", c) }}
}

var controlRoot = "/tmp"

// controlPath returns a short, per-process control-socket path for host, in a
// directory under /tmp (short, for the ~104-byte socket path limit) that only
// this user can use, or "" if another user owns that directory.
func controlPath(host string) string {
	dir := filepath.Join(controlRoot, fmt.Sprintf("gru-%d", os.Getuid()))
	os.Mkdir(dir, 0o700)
	fi, err := os.Lstat(dir)
	if err != nil {
		return ""
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || !fi.IsDir() || fi.Mode().Perm() != 0o700 || int(st.Uid) != os.Getuid() {
		return ""
	}
	h := fnv.New32a()
	h.Write([]byte(host))
	return filepath.Join(dir, fmt.Sprintf("cm-%x-%d", h.Sum32(), os.Getpid()))
}

func New(host string) *Client {
	host = strings.TrimSpace(host)
	// ControlMaster=no: commands never create a master themselves (that would
	// daemonize and hang Wait); they only multiplex over an existing one.
	c := &Client{Host: host, opts: []string{"-o", "ControlMaster=no"}}
	// Reuse a live master from the user's ~/.ssh/config if there is one (e.g. an
	// interactive login on a 2FA host, which a non-interactive master can't pass);
	// `-O check` only probes the local socket. Otherwise use our own socket.
	if exec.Command("ssh", "-O", "check", host).Run() != nil {
		if c.ctlPath = controlPath(host); c.ctlPath != "" {
			c.opts = append(c.opts, "-o", "ControlPath="+c.ctlPath)
		}
	}
	c.cmd = func(command string) *exec.Cmd {
		// RequestTTY=no: some hosts force RequestTTY=yes in ~/.ssh/config, which
		// allocates a pty and corrupts piped command I/O (the sync byte stream).
		args := append(append([]string{}, c.opts...), "-o", "RequestTTY=no", host, command)
		return exec.Command("ssh", args...)
	}
	return c
}

// Open starts a shared control master so subsequent commands multiplex over a
// single ssh connection (one handshake instead of one per command — a big win
// through a jumphost). Best-effort: if it fails, commands still run, each
// connecting directly. Call Close when done.
func (c *Client) Open() {
	if c.ctlPath == "" {
		return
	}
	// nil stdio → /dev/null, so the master holds no command pipe and Wait never
	// blocks on it.
	m := exec.Command("ssh", "-o", "ControlMaster=yes", "-o", "ControlPath="+c.ctlPath, "-N", c.Host)
	if err := m.Start(); err != nil {
		return
	}
	c.master = m

	// A host needing a password/2FA makes the non-interactive `ssh -N` exit right
	// away: watch for that exit to fall back to direct connections at once. This
	// goroutine owns m.Wait(); Close() reaps via the channel.
	c.masterDone = make(chan struct{})
	go func() { m.Wait(); close(c.masterDone) }()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if exec.Command("ssh", "-o", "ControlPath="+c.ctlPath, "-O", "check", c.Host).Run() == nil {
			return
		}
		select {
		case <-c.masterDone:
			c.master = nil
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Close tears down the control master opened by Open (no-op otherwise) and
// removes its socket.
func (c *Client) Close() {
	if c.master == nil {
		return
	}
	exec.Command("ssh", "-o", "ControlPath="+c.ctlPath, "-O", "exit", c.Host).Run()
	c.master.Process.Kill()
	<-c.masterDone
	os.Remove(c.ctlPath)
	c.master = nil
}

// Run runs command, wiring the given streams, and returns its exit code. A
// negative code with a non-nil error means the command (or ssh) didn't run.
func (c *Client) Run(command string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := c.cmd(command)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return ExitCode(cmd.Run())
}

// RunStdout runs command and hands its stdout stream to consume, returning the
// command's exit code. Used to receive a file stream from the agent.
func (c *Client) RunStdout(command string, stderr io.Writer, consume func(io.Reader) error) (int, error) {
	cmd := c.cmd(command)
	cmd.Stderr = stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return -1, err
	}
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	cErr := consume(out)
	wErr := cmd.Wait()
	if cErr != nil {
		return -1, cErr
	}
	return ExitCode(wErr)
}

// Output runs command and returns its trimmed stdout.
func (c *Client) Output(command string) (string, error) {
	var out, errb bytes.Buffer
	cmd := c.cmd(command)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %v: %s", c.Host, err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// Interactive opens an interactive ssh session with a tty, running command.
func (c *Client) Interactive(command string) (int, error) {
	cmd := exec.Command("ssh", append(append([]string{}, c.opts...), "-t", c.Host, command)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return ExitCode(cmd.Run())
}

// ExitCode maps an exec error to (code, err): nil→(0,nil), non-zero exit→(code,nil),
// failure to run→(-1,err).
func ExitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return -1, err
}

// AgentBinary returns the local gru binary for platform ("goos/goarch"): this
// executable for its own platform, else a sibling gru-<goos>-<goarch> (from
// `make cross`) next to it or in ../libexec/gru.
func AgentBinary(platform string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	return agentBinary(self, platform)
}

func agentBinary(self, platform string) (string, error) {
	if platform == runtime.GOOS+"/"+runtime.GOARCH {
		return self, nil
	}
	name := "gru-" + strings.ReplaceAll(platform, "/", "-")
	for _, p := range []string{filepath.Join(filepath.Dir(self), name), filepath.Join(filepath.Dir(self), "..", "libexec", "gru", name)} {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p, nil
		}
	}
	return "", fmt.Errorf("no gru binary for %s (looked for %s); build it with `make cross`", platform, name)
}

// EnsureAgent makes sure the agent for the remote's platform (from binaryFor) is
// in the remote's stateDir and returns its invocation, as one shell word. Each
// build has its own file, named by version, platform and a hash of the binary,
// so an upload never changes the agent a running job calls.
func (c *Client) EnsureAgent(stateDir, version string, binaryFor func(platform string) (string, error)) (string, error) {
	unm, err := c.Output("uname -sm")
	if err != nil {
		return "", err
	}
	platform := unameToGo(unm)
	bin, err := binaryFor(platform)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(bin)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	dir := remote.RemotePath(stateDir) + "/bin"
	invoke := dir + "/" + remote.ShellQuote(fmt.Sprintf("gru-%s-%s-%s", version, strings.ReplaceAll(platform, "/", "-"), hex.EncodeToString(sum[:6])))
	if _, err := c.Output("test -x " + invoke); err == nil {
		return invoke, nil
	}
	// Upload to a temp file and rename, so nobody runs a half-written agent.
	upload := fmt.Sprintf("(umask 077 && mkdir -p %[1]s) && t=%[1]s/.upload.$$ && "+
		"cat > \"$t\" && chmod 755 \"$t\" && mv -f \"$t\" %[2]s || { rm -f \"$t\"; exit 1; }", dir, invoke)
	var errb bytes.Buffer
	if code, err := c.Run(upload, bytes.NewReader(b), io.Discard, &errb); code != 0 {
		return "", fmt.Errorf("upload to %s: exit %d %v: %s", c.Host, code, err, strings.TrimSpace(errb.String()))
	}
	if v, err := c.Output(invoke + " _agent version"); err != nil || v != "gru "+version {
		return "", fmt.Errorf("agent %s reports %q (%v), want %q", invoke, v, err, "gru "+version)
	}
	return invoke, nil
}

// unameToGo maps `uname -sm` output to a GOOS/GOARCH string.
func unameToGo(unm string) string {
	f := strings.Fields(strings.ToLower(unm))
	if len(f) != 2 {
		return unm
	}
	arch := map[string]string{"aarch64": "arm64", "x86_64": "amd64"}[f[1]]
	return f[0] + "/" + cmp.Or(arch, f[1])
}
