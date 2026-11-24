// Package remote builds the shell snippets that run a job on a remote host in a
// detached, dependency-free way: setsid/nohup + a log file + a status file. No
// tmux or other remote tooling is required beyond a POSIX shell and coreutils.
package remote

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Job is a task command, its lifecycle hooks and environment, and its identity.
type Job struct {
	ID        string
	Task      string
	Host      string
	Repo      string // the local project's name, for `ps`
	Git       string // local commit SHA of the synced code
	Dirty     bool   // was the working tree dirty at launch
	Env       []string
	Command   string
	Args      string // per-run task text, exported to the command/hooks as $GRU_ARGS
	Before    []string
	OnSuccess []string
	OnFailure []string
	Always    []string
	Agent     string // the gru agent on the target, as one shell word
	Minion    string // minion on the target, as one shell word, or ""
	Notify    string // the resolved notify block as one line of JSON; the agent sends it at job end
}

// ShellQuote single-quotes s for safe use as a shell argument.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// RemotePath renders a remote path as a single shell word: a leading ~ expands to
// $HOME and $NAME / ${NAME} references expand on the remote (e.g. /scratch/$USER);
// everything else is single-quoted so spaces and shell metacharacters stay
// literal.
func RemotePath(p string) string {
	if p == "~" {
		return `"$HOME"`
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
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

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// CheckID refuses a job id that isn't a plain name, so it can't escape the jobs
// dir or reach the shell.
func CheckID(id string) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("bad job id %q", id)
	}
	return nil
}

// JobsDir returns the jobs directory under a host's state dir, as one shell word.
func JobsDir(stateDir string) string { return RemotePath(stateDir) + "/jobs" }

// JobDir returns a job's directory, as one shell word (ids pass CheckID).
func JobDir(stateDir, id string) string { return JobsDir(stateDir) + "/" + id }

// IDTaken is LaunchCommand's exit code when the job's directory already exists.
const IDTaken = 73

// waits starts each sh -c of a job: on a kill, that sh waits for what it runs,
// as the wrapper does. dash forks even a lone command and would die at once,
// letting the wrapper end the job while the task is still stopping.
const waits = "trap 'exit 143' TERM INT HUP; "

// Script builds the sh wrapper of a job. The command and each hook run in their
// own `sh -c`, so their syntax or an exit can't break the wrapper. Whatever ends
// the job (the command, a failing before hook, a kill, a failed cd), the EXIT
// trap runs on_success or on_failure, then always, records the status and
// run.json, and notifies last, so a slow notify never changes the outcome.
func Script(workDir, jobDir string, j Job) string {
	var b strings.Builder
	run := func(cmds []string, suffix string) {
		for _, c := range cmds {
			fmt.Fprintf(&b, "  sh -c %s%s\n", ShellQuote(waits+c), suffix)
		}
	}
	for _, e := range j.Env {
		// double-quoted so spaces survive while $VAR still expands.
		if k, v, ok := strings.Cut(e, "="); ok {
			fmt.Fprintf(&b, "export %s=\"%s\"\n", k, v)
		} else {
			fmt.Fprintf(&b, "export %s\n", e)
		}
	}
	fmt.Fprintf(&b, "J=%s\n", jobDir)
	fmt.Fprintf(&b, "export GRU_ARGS=%s GRU_AGENT=%s GRU_MINION=%s GRU_JOB_ID=%s GRU_TASK=%s GRU_HOST=%s GRU_GIT=%s GRU_DIRTY=%t\n",
		ShellQuote(j.Args), j.Agent, j.Minion, ShellQuote(j.ID), ShellQuote(j.Task), ShellQuote(j.Host), ShellQuote(j.Git), j.Dirty)
	fmt.Fprintf(&b, "printf %%s %s > \"$J/task\"; printf %%s %s > \"$J/repo\"\n", ShellQuote(j.Task), ShellQuote(j.Repo))
	b.WriteString("ec=1 start=$(date +%s) start_ns=$(\"$GRU_AGENT\" _agent now)\n")
	if j.Notify != "" {
		// A file, not the environment, so its secrets reach only who reads it.
		fmt.Fprintf(&b, "(umask 077 && cat > \"$J/notify.json\") <<'GRU_EOF'\n%s\nGRU_EOF\n", j.Notify)
		b.WriteString("export GRU_NOTIFY=\"$J/notify.json\"\n")
	}
	b.WriteString("finish() {\n  trap '' TERM INT HUP\n")
	b.WriteString("  export GRU_EXIT=$ec GRU_DURATION=$(( $(date +%s) - start ))\n")
	b.WriteString("  if [ $ec -eq 0 ]; then\n  export GRU_STATUS=ok\n")
	run(j.OnSuccess, "")
	b.WriteString("  else\n  export GRU_STATUS=fail\n")
	run(j.OnFailure, "")
	b.WriteString("  fi\n")
	run(j.Always, "")
	b.WriteString("  echo $ec > \"$J/status\"\n")
	meta, _ := json.Marshal(map[string]any{"id": j.ID, "task": j.Task, "host": j.Host, "git": j.Git, "dirty": j.Dirty})
	fmt.Fprintf(&b, "  printf '%%s,\"hostname\":\"%%s\",\"kernel\":\"%%s\",\"start_ns\":%%s,\"end_ns\":%%s,\"exit\":%%s}\\n' %s "+
		"\"$(uname -n)\" \"$(uname -r)\" \"$start_ns\" \"$(\"$GRU_AGENT\" _agent now)\" $ec > \"$J/run.json\"\n",
		ShellQuote(strings.TrimSuffix(string(meta), "}")))
	if j.Notify != "" {
		b.WriteString("  \"$GRU_AGENT\" _agent notify --config \"$GRU_NOTIFY\" --log \"$J/log\"\n")
	}
	b.WriteString("  exit $ec\n}\ntrap finish EXIT\ntrap 'ec=143; echo \"[gru] killed\"; exit' TERM INT HUP\n")
	fmt.Fprintf(&b, "cd %s || exit\n", RemotePath(workDir))
	run(j.Before, " || { ec=$?; exit; }")
	fmt.Fprintf(&b, "sh -c %s\nec=$?\n", ShellQuote(waits+j.Command))
	return b.String()
}

// LaunchCommand creates the job's directory (failing with IDTaken if it
// exists), records this host's name, saves the script (read from stdin) and
// starts it detached, recording its pid. umask 077 keeps the directory and the
// script, which may hold secrets, private. The job survives ssh disconnect via
// setsid (or nohup).
func LaunchCommand(stateDir, id string) string {
	return fmt.Sprintf(
		"(umask 077 && mkdir -p %[1]s && { [ ! -e %[2]s ] || exit %[3]d; } && mkdir %[2]s && "+
			"uname -n > %[2]s/hostname && cat > %[2]s/run.sh) && "+
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

// KillCommand sends SIGTERM to a running job's process group (or, without
// setsid, its wrapper); the wrapper records the status. It refuses a finished
// job and a job of another host sharing the state dir: their pid means nothing.
func KillCommand(jobDir string) string {
	return fmt.Sprintf(
		"%[2]s || { echo 'gru: no such job on this host' >&2; exit 1; }; "+
			"[ ! -e %[1]s/status ] || { echo 'gru: the job has finished' >&2; exit 1; }; "+
			"p=$(cat %[1]s/pid) && { kill -- -$p 2>/dev/null || kill $p; } && echo \"killed $p\"",
		jobDir, onThisHost(jobDir))
}

// PsCommand lists this host's jobs, newest launch first, as tab-separated id,
// status, repo and task. A job without a status whose wrapper is gone is "lost".
func PsCommand(stateDir string) string {
	return fmt.Sprintf(
		"cd %s 2>/dev/null && for h in $(ls -1t */hostname 2>/dev/null); do d=${h%%/hostname}; %s || continue; "+
			"if [ -e \"$d/status\" ]; then st=\"exit $(cat \"$d/status\")\"; "+
			"elif kill -0 \"$(cat \"$d/pid\" 2>/dev/null)\" 2>/dev/null; then st=running; else st=lost; fi; "+
			"printf '%%s\\t%%s\\t%%s\\t%%s\\n' \"$d\" \"$st\" \"$(cat \"$d/repo\" 2>/dev/null)\" \"$(cat \"$d/task\" 2>/dev/null)\"; done; true",
		JobsDir(stateDir), onThisHost(`"$d"`))
}
