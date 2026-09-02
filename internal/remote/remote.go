// Package remote builds the shell snippets that run a job on a remote host in a
// detached, dependency-free way: setsid/nohup + a log file + a status file. No
// tmux or other remote tooling is required beyond a POSIX shell and coreutils.
package remote

import (
	"fmt"
	"strconv"
	"strings"
)

// Job is a task command plus its lifecycle hooks and environment.
type Job struct {
	Dir       string
	Env       []string
	Command   string
	Before    []string
	OnSuccess []string
	OnFailure []string
	Always    []string
}

// sq single-quotes s for safe use as a shell argument.
func sq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

const jobsDir = ".minion/jobs"

// JobDir returns the remote job directory (with ~ for the remote shell to expand).
func JobDir(id string) string { return "~/" + jobsDir + "/" + id }

// Meta is provenance recorded to <jobDir>/run.json for reproducibility.
type Meta struct {
	ID    string
	Task  string
	Host  string
	Git   string // local commit SHA of the synced code
	Dirty bool   // was the working tree dirty at launch
}

// Script builds the sh wrapper that runs the job's hooks and command, recording
// the exit code to <jobDir>/status. Hook order mirrors the local runner:
// before → command → on_success|on_failure → always; a failing before hook
// runs always (teardown) and exits.
func Script(workDir, jobDir string, j Job, m Meta) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	fmt.Fprintf(&b, "cd %s || exit 1\n", workDir)
	for _, e := range j.Env {
		// double-quote the value so spaces survive while $VAR still expands.
		if k, v, ok := strings.Cut(e, "="); ok {
			fmt.Fprintf(&b, "export %s=\"%s\"\n", k, v)
		} else {
			fmt.Fprintf(&b, "export %s\n", e)
		}
	}
	b.WriteString("start=$(date +%s)\n")
	// record start time and task so `ps` can report them (the id is a random name).
	fmt.Fprintf(&b, "echo \"$start\" > %s/started\n", jobDir)
	fmt.Fprintf(&b, "printf %%s %s > %s/task\n", sq(m.Task), jobDir)

	// Commands run in ( ) subshells so a task calling exit/cd can't terminate
	// the wrapper before it records the status.
	b.WriteString("fail() { ")
	for _, c := range j.Always {
		fmt.Fprintf(&b, "( %s ); ", c)
	}
	fmt.Fprintf(&b, "echo \"$1\" > %s/status; exit \"$1\"; }\n", jobDir)

	for _, c := range j.Before {
		fmt.Fprintf(&b, "( %s ) || fail $?\n", c)
	}

	fmt.Fprintf(&b, "( %s )\nec=$?\n", j.Command)
	// `:` keeps each branch non-empty even when a hook list is empty.
	b.WriteString("if [ \"$ec\" -eq 0 ]; then\n  :\n")
	for _, c := range j.OnSuccess {
		fmt.Fprintf(&b, "  ( %s )\n", c)
	}
	b.WriteString("else\n  :\n")
	for _, c := range j.OnFailure {
		fmt.Fprintf(&b, "  ( %s )\n", c)
	}
	b.WriteString("fi\n")
	for _, c := range j.Always {
		fmt.Fprintf(&b, "( %s )\n", c)
	}
	fmt.Fprintf(&b, "echo \"$ec\" > %s/status\n", jobDir)
	// Values go in as shell-quoted printf args (not baked into the format), so a
	// % or ' in a task/host/git string can't corrupt the JSON.
	fmt.Fprintf(&b,
		"printf '{\"id\":\"%%s\",\"task\":\"%%s\",\"host\":\"%%s\",\"git\":\"%%s\",\"dirty\":%%s,\"start\":%%s,\"end\":%%s,\"exit\":%%s}\\n' %s %s %s %s %s \"$start\" \"$(date +%%s)\" \"$ec\" > %s/run.json\n",
		sq(m.ID), sq(m.Task), sq(m.Host), sq(m.Git), sq(strconv.FormatBool(m.Dirty)), jobDir)
	b.WriteString("exit \"$ec\"\n")
	return b.String()
}

// LaunchCommand saves the script (read from stdin) and starts it detached,
// recording its pid. The job survives ssh disconnect via setsid (or nohup).
func LaunchCommand(jobDir string) string {
	return fmt.Sprintf(
		"mkdir -p %[1]s && cat > %[1]s/run.sh && chmod +x %[1]s/run.sh && "+
			"{ if command -v setsid >/dev/null 2>&1; then setsid sh %[1]s/run.sh </dev/null >> %[1]s/log 2>&1 & "+
			"else nohup sh %[1]s/run.sh </dev/null >> %[1]s/log 2>&1 & fi; echo $! > %[1]s/pid; }",
		jobDir)
}

// AttachCommand streams the job log from the start and follows until the job
// finishes, then exits with the job's recorded status.
func AttachCommand(jobDir string) string {
	return fmt.Sprintf(
		"[ -e %[1]s/log ] || { echo 'minion: no such job' >&2; exit 1; }; "+
			"tail -n +1 -f %[1]s/log & tp=$!; "+
			"while [ ! -e %[1]s/status ]; do sleep 1; done; sleep 1; kill $tp 2>/dev/null; "+
			"exit $(cat %[1]s/status 2>/dev/null || echo 0)",
		jobDir)
}

// KillCommand stops a running job (its process group, then the pid).
func KillCommand(jobDir string) string {
	return fmt.Sprintf(
		"p=$(cat %[1]s/pid 2>/dev/null); "+
			"if [ -n \"$p\" ]; then kill -- -$p 2>/dev/null; kill $p 2>/dev/null; echo \"killed $p\"; "+
			"else echo 'minion: no pid' >&2; exit 1; fi",
		jobDir)
}

// LatestJobCommand prints the most recently active job id (by mtime), if any.
func LatestJobCommand() string {
	return fmt.Sprintf("ls -1t ~/%s 2>/dev/null | head -1", jobsDir)
}

// PsCommand lists jobs as tab-separated id, task, started-epoch, and status.
func PsCommand() string {
	return fmt.Sprintf(
		"for d in ~/%s/*/; do [ -d \"$d\" ] || continue; id=$(basename \"$d\"); "+
			"if [ -e \"$d/status\" ]; then st=\"exit $(cat \"$d/status\")\"; else st=running; fi; "+
			"printf '%%s\\t%%s\\t%%s\\t%%s\\n' \"$id\" \"$(cat \"$d/task\" 2>/dev/null)\" \"$(cat \"$d/started\" 2>/dev/null)\" \"$st\"; done",
		jobsDir)
}
