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
	Args      string // per-run task text, exported to the command/hooks as $MINION_ARGS
	Before    []string
	OnSuccess []string
	OnFailure []string
	Always    []string
	Notify    string // shell run once at job end (after status is recorded), with $MINION_* in the environment
}

// ShellQuote single-quotes s for safe use as a shell argument.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// RemotePath renders a remote path as a single shell word: a leading ~ expands to
// $HOME, the remainder is single-quoted so spaces and shell metacharacters stay
// literal. Absolute and $HOME-relative paths are quoted whole. This lets
// remote_dir / repo names contain spaces without breaking the ssh command string.
func RemotePath(p string) string {
	if p == "~" {
		return `"$HOME"`
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		// Adjacent quoted segments concatenate into one word: $HOME/<rest>.
		return `"$HOME"/` + ShellQuote(rest)
	}
	return ShellQuote(p)
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
	fmt.Fprintf(&b, "cd %s || exit 1\n", RemotePath(workDir))
	for _, e := range j.Env {
		// double-quote the value so spaces survive while $VAR still expands.
		if k, v, ok := strings.Cut(e, "="); ok {
			fmt.Fprintf(&b, "export %s=\"%s\"\n", k, v)
		} else {
			fmt.Fprintf(&b, "export %s\n", e)
		}
	}
	if j.Args != "" {
		// single-quoted so the task text is passed literally (no expansion/injection).
		fmt.Fprintf(&b, "export MINION_ARGS=%s\n", ShellQuote(j.Args))
	}
	b.WriteString("start=$(date +%s)\n")
	// record start time and task so `ps` can report them (the id is a random name).
	fmt.Fprintf(&b, "echo \"$start\" > %s/started\n", jobDir)
	fmt.Fprintf(&b, "printf %%s %s > %s/task\n", ShellQuote(m.Task), jobDir)
	fmt.Fprintf(&b, "basename \"$PWD\" > %s/repo\n", jobDir) // for `ps` per-package filtering

	// Job identity exported for hooks and notifications; MINION_EXIT/STATUS/
	// DURATION are filled in once the command finishes (below).
	fmt.Fprintf(&b, "export MINION_JOB_ID=%s\n", ShellQuote(m.ID))
	fmt.Fprintf(&b, "export MINION_TASK=%s\n", ShellQuote(m.Task))
	fmt.Fprintf(&b, "export MINION_HOST=%s\n", ShellQuote(m.Host))

	// Commands run in ( ) subshells so a task calling exit/cd can't terminate
	// the wrapper before it records the status. teardown() runs the `always`
	// hooks and is invoked on the normal path, on before-failure, and on signal.
	b.WriteString("teardown() { ")
	for _, c := range j.Always {
		fmt.Fprintf(&b, "( %s ); ", c)
	}
	b.WriteString(":; }\n")
	fmt.Fprintf(&b, "fail() { teardown; echo \"$1\" > %s/status; exit \"$1\"; }\n", jobDir)
	// On kill/hangup, record a status (128+SIGTERM) and note the log, so the job
	// doesn't stay "running" forever and the log shows it was killed.
	fmt.Fprintf(&b, "trap 'echo \"[minion] killed\"; teardown; echo 143 > %s/status; exit 143' TERM INT HUP\n", jobDir)

	for _, c := range j.Before {
		fmt.Fprintf(&b, "( %s ) || fail $?\n", c)
	}

	fmt.Fprintf(&b, "( %s )\nec=$?\n", j.Command)
	// Structured outcome for on_success/on_failure/always hooks and notify.
	b.WriteString("export MINION_EXIT=\"$ec\"\n")
	b.WriteString("if [ \"$ec\" -eq 0 ]; then export MINION_STATUS=ok; else export MINION_STATUS=fail; fi\n")
	b.WriteString("export MINION_DURATION=\"$(( $(date +%s) - start ))\"\n")
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
	b.WriteString("teardown\n")
	fmt.Fprintf(&b, "echo \"$ec\" > %s/status\n", jobDir)
	// Values go in as shell-quoted printf args (not baked into the format), so a
	// % or ' in a task/host/git string can't corrupt the JSON.
	fmt.Fprintf(&b,
		"printf '{\"id\":\"%%s\",\"task\":\"%%s\",\"host\":\"%%s\",\"git\":\"%%s\",\"dirty\":%%s,\"start\":%%s,\"end\":%%s,\"exit\":%%s}\\n' %s %s %s %s %s \"$start\" \"$(date +%%s)\" \"$ec\" > %s/run.json\n",
		ShellQuote(m.ID), ShellQuote(m.Task), ShellQuote(m.Host), ShellQuote(m.Git), ShellQuote(strconv.FormatBool(m.Dirty)), jobDir)
	// Notify runs last, after the outcome is recorded, so a slow or failing
	// notification never changes the job's exit status.
	if j.Notify != "" {
		b.WriteString(j.Notify)
		b.WriteString("\n")
	}
	b.WriteString("exit \"$ec\"\n")
	return b.String()
}

// LaunchCommand saves the script (read from stdin) and starts it detached,
// recording its pid. The job survives ssh disconnect via setsid (or nohup).
func LaunchCommand(jobDir string) string {
	return fmt.Sprintf(
		"mkdir -p %[1]s && cat > %[1]s/run.sh && chmod 700 %[1]s/run.sh && "+
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
			"if [ -n \"$p\" ]; then kill -- -$p 2>/dev/null; kill $p 2>/dev/null; "+
			"echo '[minion] killed' >> %[1]s/log 2>/dev/null; "+
			"[ -e %[1]s/status ] || echo 143 > %[1]s/status; "+ // ensure it's not stuck "running"
			"echo \"killed $p\"; "+
			"else echo 'minion: no pid' >&2; exit 1; fi",
		jobDir)
}

// LatestJobCommand prints the most recently active job id (by mtime), if any.
func LatestJobCommand() string {
	return fmt.Sprintf("ls -1t ~/%s 2>/dev/null | head -1", jobsDir)
}

// PsCommand lists jobs as tab-separated id, task, started-epoch, status, repo.
func PsCommand() string {
	return fmt.Sprintf(
		"for d in ~/%s/*/; do [ -d \"$d\" ] || continue; id=$(basename \"$d\"); "+
			"if [ -e \"$d/status\" ]; then st=\"exit $(cat \"$d/status\")\"; else st=running; fi; "+
			"printf '%%s\\t%%s\\t%%s\\t%%s\\t%%s\\n' \"$id\" \"$(cat \"$d/task\" 2>/dev/null)\" \"$(cat \"$d/started\" 2>/dev/null)\" \"$st\" \"$(cat \"$d/repo\" 2>/dev/null)\"; done",
		jobsDir)
}
