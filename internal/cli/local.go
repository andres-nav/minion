package cli

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// execer runs shell commands somewhere — remotely via ssh, or on this machine.
// *ssh.Client satisfies it structurally; localExec is the local implementation.
type execer interface {
	Run(command string, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	Output(command string) (string, error)
	RunStdout(command string, stderr io.Writer, consume func(io.Reader) error) (int, error)
}

// localExec runs commands on the local machine via `sh -c`, so the same job
// model (detached run, log/status files, ps/attach/kill) works locally.
type localExec struct{}

func (localExec) Run(command string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := exec.Command("sh", "-c", command)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return exitCode(cmd.Run())
}

func (localExec) Output(command string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.Command("sh", "-c", command)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("local: %s: %v: %s", command, err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func (localExec) RunStdout(command string, stderr io.Writer, consume func(io.Reader) error) (int, error) {
	cmd := exec.Command("sh", "-c", command)
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
	return exitCode(wErr)
}

func exitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return -1, err
}

// isLocalAlias reports whether an ssh alias means "run on this machine".
func isLocalAlias(a string) bool {
	return a == "localhost" || a == "local"
}
