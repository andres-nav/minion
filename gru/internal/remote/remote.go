// Package remote builds the shell snippets that run a job on a remote host in a
// detached, dependency-free way: setsid/nohup + a log file + a status file. No
// tmux or other remote tooling is required beyond a POSIX shell and coreutils.
package remote

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Job is a task command plus its lifecycle hooks and environment.
type Job struct {
	Dir       string
	Env       []string
	Command   string
	Args      string // per-run task text, exported to the command/hooks as $GRU_ARGS
	Before    []string
	OnSuccess []string
	OnFailure []string
	Always    []string
	Agent     string // the gru agent on the target, as one shell word
	Notify    string // the resolved notify block as one line of JSON; the agent sends it at job end
}

// ShellQuote single-quotes s for safe use as a shell argument.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// RemotePath renders a remote path as a single shell word: a leading ~ expands to
// $HOME and $NAME / ${NAME} references expand on the remote (e.g. /scratch/$USER);
// everything else is single-quoted so spaces and shell metacharacters stay
// literal. This lets remote_dir / repo names contain spaces without breaking the
// ssh command string.
func RemotePath(p string) string {
	if p == "~" {
		return `"$HOME"`
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		// Adjacent quoted segments concatenate into one word: $HOME/<rest>.
		return `"$HOME"/` + quoteExpandingVars(rest)
	}
	return quoteExpandingVars(p)
}

var envRef = regexp.MustCompile(`\$(\{[A-Za-z_][A-Za-z0-9_]*\}|[A-Za-z_][A-Za-z0-9_]*)`)

// quoteExpandingVars single-quotes s, then reopens the quotes around each variable
// reference as "${NAME:?}": expanded by the remote shell, and an error if unset or
// empty so a missing variable can't silently turn the path into another directory.
func quoteExpandingVars(s string) string {
	return envRef.ReplaceAllStringFunc(ShellQuote(s), func(ref string) string {
		return `'"${` + strings.Trim(ref[1:], "{}") + `:?}"'`
	})
}

// JobsDir returns the jobs directory under a host's state dir, as one shell word.
func JobsDir(stateDir string) string { return RemotePath(stateDir) + "/jobs" }

// JobDir returns a job's directory, as one shell word (ids are [A-Za-z0-9_-]).
func JobDir(stateDir, id string) string { return JobsDir(stateDir) + "/" + id }

// IDTaken is LaunchCommand's exit code when the job's directory already exists.
const IDTaken = 73

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
		fmt.Fprintf(&b, "export GRU_ARGS=%s\n", ShellQuote(j.Args))
	}
	fmt.Fprintf(&b, "export GRU_AGENT=%s\n", j.Agent)
	b.WriteString("start=$(date +%s)\n")
	b.WriteString("start_ns=$(\"$GRU_AGENT\" _agent now)\n")
	// record start time and task so `ps` can report them (the id is a random name).
	fmt.Fprintf(&b, "echo \"$start\" > %s/started\n", jobDir)
	fmt.Fprintf(&b, "printf %%s %s > %s/task\n", ShellQuote(m.Task), jobDir)
	fmt.Fprintf(&b, "basename \"$PWD\" > %s/repo\n", jobDir) // for `ps` per-package filtering

	// Job identity exported for hooks and notifications; GRU_EXIT/STATUS/
	// DURATION are filled in once the command finishes (below).
	fmt.Fprintf(&b, "export GRU_JOB_ID=%s\n", ShellQuote(m.ID))
	fmt.Fprintf(&b, "export GRU_TASK=%s\n", ShellQuote(m.Task))
	fmt.Fprintf(&b, "export GRU_HOST=%s\n", ShellQuote(m.Host))
	fmt.Fprintf(&b, "export GRU_GIT=%s GRU_DIRTY=%t\n", ShellQuote(m.Git), m.Dirty)
	if j.Notify != "" {
		// A file, not the environment, so the secrets in it reach only who reads
		// it: the agent at job end, or a task that sends its own notifications.
		fmt.Fprintf(&b, "(umask 077 && cat > %s/notify.json) <<'GRU_EOF'\n%s\nGRU_EOF\n", jobDir, j.Notify)
		fmt.Fprintf(&b, "export GRU_NOTIFY=%s/notify.json\n", jobDir)
	}

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
	fmt.Fprintf(&b, "killed() { echo \"[gru] killed\"; teardown; echo 143 > %s/status; exit 143; }\n", jobDir)
	b.WriteString("trap killed TERM INT HUP\n")

	for _, c := range j.Before {
		fmt.Fprintf(&b, "( %s ) || fail $?\n", c)
	}

	fmt.Fprintf(&b, "( %s )\nec=$?\n", j.Command)
	// Structured outcome for on_success/on_failure/always hooks and notify.
	b.WriteString("export GRU_EXIT=\"$ec\"\n")
	b.WriteString("if [ \"$ec\" -eq 0 ]; then export GRU_STATUS=ok; else export GRU_STATUS=fail; fi\n")
	b.WriteString("export GRU_DURATION=\"$(( $(date +%s) - start ))\"\n")
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
		"printf '{\"id\":\"%%s\",\"task\":\"%%s\",\"host\":\"%%s\",\"hostname\":\"%%s\",\"kernel\":\"%%s\",\"git\":\"%%s\",\"dirty\":%%s,\"start_ns\":%%s,\"end_ns\":%%s,\"exit\":%%s}\\n' %s %s %s \"$(uname -n)\" \"$(uname -r)\" %s %s \"$start_ns\" \"$(\"$GRU_AGENT\" _agent now)\" \"$ec\" > %s/run.json\n",
		ShellQuote(m.ID), ShellQuote(m.Task), ShellQuote(m.Host), ShellQuote(m.Git), ShellQuote(strconv.FormatBool(m.Dirty)), jobDir)
	// Notify runs last, after the outcome is recorded, so a slow or failing
	// notification never changes the job's exit status.
	if j.Notify != "" {
		fmt.Fprintf(&b, "\"$GRU_AGENT\" _agent notify --config \"$GRU_NOTIFY\" --log %s/log\n", jobDir)
	}
	b.WriteString("exit \"$ec\"\n")
	return b.String()
}

// LaunchCommand creates the job's directory (failing with IDTaken if it
// exists), records this host's name, saves the script (read from stdin) and
// starts it detached, recording its pid. umask 077 keeps the directory and the
// script, which may hold secrets, private. The job survives ssh disconnect via
// setsid (or nohup).
func LaunchCommand(stateDir, id string) string {
	return fmt.Sprintf(
		"(umask 077 && mkdir -p %[1]s && { mkdir %[2]s 2>/dev/null || exit %[3]d; } && "+
			"uname -n > %[2]s/hostname && cat > %[2]s/run.sh) && chmod 700 %[2]s/run.sh && "+
			"{ if command -v setsid >/dev/null 2>&1; then setsid sh %[2]s/run.sh </dev/null >> %[2]s/log 2>&1 & "+
			"else nohup sh %[2]s/run.sh </dev/null >> %[2]s/log 2>&1 & fi; echo $! > %[2]s/pid; }",
		JobsDir(stateDir), JobDir(stateDir, id), IDTaken)
}

// AttachCommand streams the job log from the start and follows until the job
// finishes, then exits with the job's recorded status. agent is the gru
// agent on the target, as one shell word.
func AttachCommand(agent, jobDir string) string {
	return fmt.Sprintf("[ -e %[2]s/log ] || { echo 'gru: no such job' >&2; exit 1; }; %[1]s _agent follow %[2]s", agent, jobDir)
}

// onThisHost is a shell test: the job in dir was launched on this host.
func onThisHost(dir string) string {
	return `[ "$(cat ` + dir + `/hostname 2>/dev/null)" = "$(uname -n)" ]`
}

// KillCommand stops a running job (its process group, then the pid). It refuses
// a job of another host sharing the state dir: its pid means nothing here.
func KillCommand(jobDir string) string {
	return fmt.Sprintf(
		"%[2]s || { echo 'gru: no such job on this host' >&2; exit 1; }; "+
			"p=$(cat %[1]s/pid 2>/dev/null); "+
			"if [ -n \"$p\" ]; then kill -- -$p 2>/dev/null; kill $p 2>/dev/null; "+
			"echo '[gru] killed' >> %[1]s/log 2>/dev/null; "+
			"[ -e %[1]s/status ] || echo 143 > %[1]s/status; "+ // ensure it's not stuck "running"
			"echo \"killed $p\"; "+
			"else echo 'gru: no pid' >&2; exit 1; fi",
		jobDir, onThisHost(jobDir))
}

// LatestJobCommand prints this host's most recently active job id (by mtime), if any.
func LatestJobCommand(stateDir string) string {
	return fmt.Sprintf(
		"cd %s 2>/dev/null && for d in $(ls -1t); do %s && { echo \"$d\"; break; }; done; true",
		JobsDir(stateDir), onThisHost(`"$d"`))
}

// PsCommand lists this host's jobs as tab-separated id, task, started-epoch, status, repo.
func PsCommand(stateDir string) string {
	return fmt.Sprintf(
		"for d in %s/*/; do [ -d \"$d\" ] && %s || continue; id=$(basename \"$d\"); "+
			"if [ -e \"$d/status\" ]; then st=\"exit $(cat \"$d/status\")\"; else st=running; fi; "+
			"printf '%%s\\t%%s\\t%%s\\t%%s\\t%%s\\n' \"$id\" \"$(cat \"$d/task\" 2>/dev/null)\" \"$(cat \"$d/started\" 2>/dev/null)\" \"$st\" \"$(cat \"$d/repo\" 2>/dev/null)\"; done",
		JobsDir(stateDir), onThisHost(`"$d"`))
}
