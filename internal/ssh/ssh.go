package ssh

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ResolveAgentBinary picks the local minion binary to upload for the remote's
// target platform ("goos/goarch"): the running executable serves its own
// platform; a different target uses a sibling minion-<goos>-<goarch> (from
// `make cross`). exists reports whether a candidate path is a usable file.
func ResolveAgentBinary(selfPath, selfPlatform, target string, exists func(string) bool) (string, error) {
	if target == selfPlatform {
		return selfPath, nil
	}
	goos, goarch, ok := splitPlatform(target)
	if !ok {
		return "", fmt.Errorf("unrecognized remote platform %q", target)
	}
	name := fmt.Sprintf("minion-%s-%s", goos, goarch)
	dir := filepath.Dir(selfPath)
	// Look alongside the binary (dev/build tree: bin/) then in the installed
	// libexec dir (bin/../libexec/minion), so ~/.local/bin holds only `minion`.
	for _, cand := range []string{
		filepath.Join(dir, name),
		filepath.Join(dir, "..", "libexec", "minion", name),
	} {
		if exists(cand) {
			return cand, nil
		}
	}
	return "", fmt.Errorf("no minion binary for %s (looked for %s); build it with `make cross`", target, name)
}

func splitPlatform(p string) (goos, goarch string, ok bool) {
	goos, goarch, ok = strings.Cut(p, "/")
	if !ok || goos == "" || goarch == "" {
		return "", "", false
	}
	return goos, goarch, true
}

const (
	remoteBinDir = ".minion/bin"
	remoteBin    = ".minion/bin/minion" // relative to remote $HOME
)

// Client runs commands on a remote host via the system ssh binary, inheriting
// the user's ~/.ssh/config (ProxyJump, ControlMaster, ProxyCommand, …).
type Client struct {
	Host       string
	ctlPath    string        // control-master socket path ("" when reusing the user's master)
	opts       []string      // ssh options shared by every command
	master     *exec.Cmd     // the control-master process, if Open()ed
	masterDone chan struct{} // closed when the master process exits (its Wait is owned by Open)
	ssh        func(command string) *exec.Cmd
}

// controlDir returns a private (0700) per-user directory for control sockets, so
// the socket is not created in a world-writable /tmp location another user could
// pre-empt. Kept directly under /tmp (not os.TempDir, which is long on macOS) to
// stay well under the ~104-byte unix socket path limit.
func controlDir() string {
	dir := fmt.Sprintf("/tmp/minion-%d", os.Getuid())
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

// controlPath returns a short, per-process control-socket path for host.
func controlPath(host string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(host))
	return filepath.Join(controlDir(), fmt.Sprintf("cm-%x-%d", h.Sum32(), os.Getpid()))
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
		c.ctlPath = controlPath(host)
		c.opts = append(c.opts, "-o", "ControlPath="+c.ctlPath)
	}
	c.ssh = func(command string) *exec.Cmd {
		// RequestTTY=no: some hosts (e.g. Amazon DevSpaces) force RequestTTY=yes
		// in ~/.ssh/config, which allocates a pty and corrupts piped command I/O
		// (the sync byte stream). Commands never need a tty. Interactive keeps -t.
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
		return // already multiplexing over the user's master
	}
	// nil stdio → /dev/null, so the master holds no command pipe and Wait never
	// blocks on it (the bug that forced ControlMaster=no for command connections).
	m := exec.Command("ssh", "-o", "ControlMaster=yes", "-o", "ControlPath="+c.ctlPath, "-N", c.Host)
	if err := m.Start(); err != nil {
		return
	}
	c.master = m

	// The master runs non-interactively (stdio → /dev/null), so a host needing a
	// password/2FA makes `ssh -N` exit right away. Watch for that exit so we fall
	// back to direct connections immediately instead of polling out the deadline.
	// This goroutine owns m.Wait(); Close() reaps via the channel, never Wait().
	c.masterDone = make(chan struct{})
	go func() { _ = m.Wait(); close(c.masterDone) }()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if exec.Command("ssh", "-o", "ControlPath="+c.ctlPath, "-O", "check", c.Host).Run() == nil {
			return // master is up; commands will reuse it
		}
		select {
		case <-c.masterDone:
			c.master = nil // master died; commands will connect directly
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Close tears down the control master opened by Open (no-op otherwise). It reaps
// the process by waiting on masterDone (Open's goroutine owns Wait) and removes
// the socket so a crash doesn't strand it.
func (c *Client) Close() {
	if c.master == nil {
		return
	}
	_ = exec.Command("ssh", "-o", "ControlPath="+c.ctlPath, "-O", "exit", c.Host).Run()
	_ = c.master.Process.Kill()
	<-c.masterDone
	_ = os.Remove(c.ctlPath)
	c.master = nil
}

// Run runs command on the remote, wiring the given streams, and returns its
// exit code. A negative code with a non-nil error means ssh itself failed.
func (c *Client) Run(command string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := c.ssh(command)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return ExitCode(cmd.Run())
}

// RunStdout runs command and hands its stdout stream to consume, returning the
// command's exit code. Used to receive a file stream from the remote agent.
func (c *Client) RunStdout(command string, stderr io.Writer, consume func(io.Reader) error) (int, error) {
	cmd := c.ssh(command)
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

// Interactive opens an interactive ssh session with a tty, wiring the real
// terminal. If command is non-empty it runs instead of the default login shell.
// Returns the session's exit code.
func (c *Client) Interactive(command string) (int, error) {
	a := append(append([]string{}, c.opts...), "-t", c.Host)
	if command != "" {
		a = append(a, command)
	}
	cmd := exec.Command("ssh", a...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return ExitCode(cmd.Run())
}

// ExitCode maps an exec error to (code, err): nil→(0,nil), non-zero exit→(code,nil),
// failure to run→(-1,err). Shared by the ssh and local executors.
func ExitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return -1, err
}

// Output runs command and returns its trimmed stdout.
func (c *Client) Output(command string) (string, error) {
	var out, errb bytes.Buffer
	cmd := c.ssh(command)
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ssh %s: %s: %v: %s", c.Host, command, err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func remoteInvoke() string { return "~/" + remoteBin }

// RemotePlatform returns the remote's GOOS/GOARCH (via `uname -sm`).
func (c *Client) RemotePlatform() (string, error) {
	unm, err := c.Output("uname -sm")
	if err != nil {
		return "", err
	}
	return unameToGo(unm), nil
}

// EnsureAgent makes sure a minion binary matching version is present on the
// remote and returns its invocation path. When an upload is needed it asks
// binaryFor for a local binary built for the remote's platform, so cross-arch
// hosts (e.g. a darwin laptop bootstrapping a linux box) work. A "dev" version
// is always re-uploaded.
func (c *Client) EnsureAgent(version string, binaryFor func(platform string) (string, error)) (string, error) {
	invoke := remoteInvoke()

	// "dev" and "-dirty" builds vary without the version string changing, so
	// always re-upload them; only tagged/clean versions use the fast path.
	if version != "dev" && !strings.HasSuffix(version, "-dirty") {
		if v, err := c.Output(invoke + " _agent version"); err == nil && v == "minion "+version {
			return invoke, nil
		}
	}

	platform, err := c.RemotePlatform()
	if err != nil {
		return "", err
	}
	bin, err := binaryFor(platform)
	if err != nil {
		return "", err
	}
	if err := c.upload(bin); err != nil {
		return "", err
	}

	v, err := c.Output(invoke + " _agent version")
	if err != nil {
		return "", fmt.Errorf("agent verify failed: %w", err)
	}
	if v != "minion "+version {
		return "", fmt.Errorf("remote agent reports %q, want %q", v, "minion "+version)
	}
	return invoke, nil
}

func (c *Client) upload(localBinary string) error {
	f, err := os.Open(localBinary)
	if err != nil {
		return err
	}
	defer f.Close()

	invoke := remoteInvoke()
	remoteCmd := fmt.Sprintf("mkdir -p ~/%s && cat > %s && chmod 755 %s", remoteBinDir, invoke, invoke)
	cmd := c.ssh(remoteCmd)
	cmd.Stdin = f
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("upload to %s: %v: %s", c.Host, err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// unameToGo maps `uname -sm` output to a GOOS/GOARCH string.
func unameToGo(unm string) string {
	fields := strings.Fields(unm)
	if len(fields) != 2 {
		return unm
	}
	osName := map[string]string{"Darwin": "darwin", "Linux": "linux"}[fields[0]]
	if osName == "" {
		osName = strings.ToLower(fields[0])
	}
	arch := map[string]string{"arm64": "arm64", "aarch64": "arm64", "x86_64": "amd64", "amd64": "amd64"}[fields[1]]
	if arch == "" {
		arch = fields[1]
	}
	return osName + "/" + arch
}
