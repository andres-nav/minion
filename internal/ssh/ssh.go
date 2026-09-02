package ssh

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	cand := filepath.Join(filepath.Dir(selfPath), fmt.Sprintf("minion-%s-%s", goos, goarch))
	if exists(cand) {
		return cand, nil
	}
	return "", fmt.Errorf("no minion binary for %s (looked for %s); build it with `make cross`", target, cand)
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
	Host string
	ssh  func(command string) *exec.Cmd
}

func New(host string) *Client {
	host = strings.TrimSpace(host)
	return &Client{
		Host: host,
		ssh:  func(command string) *exec.Cmd { return exec.Command("ssh", host, command) },
	}
}

// Run runs command on the remote, wiring the given streams, and returns its
// exit code. A negative code with a non-nil error means ssh itself failed.
func (c *Client) Run(command string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := c.ssh(command)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return -1, err
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
	if wErr == nil {
		return 0, nil
	}
	if ee, ok := wErr.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return -1, wErr
}

// Interactive opens an interactive ssh session with a tty, wiring the real
// terminal. If command is non-empty it runs instead of the default login shell.
// Returns the session's exit code.
func (c *Client) Interactive(command string) (int, error) {
	a := []string{"-t", c.Host}
	if command != "" {
		a = append(a, command)
	}
	cmd := exec.Command("ssh", a...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
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
