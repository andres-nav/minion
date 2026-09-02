package runner

import (
	"fmt"
	"io"
	"os"
	"os/exec"
)

// Job is a task command plus its lifecycle hooks, all shell command strings.
type Job struct {
	Dir       string
	Env       []string // KEY=VALUE, added to the process environment
	Command   string
	Before    []string
	OnSuccess []string
	OnFailure []string
	Always    []string
}

// Local runs the job locally and returns the exit code: the main command's, or
// a failing `before` hook's when setup fails before the command runs. Hooks run
// in order: before → command → on_success|on_failure → always.
func Local(j Job, stdout, stderr io.Writer) int {
	for _, c := range j.Before {
		if code := sh(j.Dir, j.Env, c, stdout, stderr); code != 0 {
			runAll(j.Dir, j.Env, j.Always, stdout, stderr)
			return code
		}
	}

	code := sh(j.Dir, j.Env, j.Command, stdout, stderr)
	if code == 0 {
		runAll(j.Dir, j.Env, j.OnSuccess, stdout, stderr)
	} else {
		runAll(j.Dir, j.Env, j.OnFailure, stdout, stderr)
	}
	runAll(j.Dir, j.Env, j.Always, stdout, stderr)
	return code
}

func runAll(dir string, env, cmds []string, stdout, stderr io.Writer) {
	for _, c := range cmds {
		sh(dir, env, c, stdout, stderr)
	}
}

// sh runs one command via `sh -c`, streaming output, and returns its exit code.
func sh(dir string, env []string, command string, stdout, stderr io.Writer) int {
	fmt.Fprintf(stderr, "+ %s\n", command)
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	fmt.Fprintf(stderr, "minion: %v\n", err)
	return 1
}
