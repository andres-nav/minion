package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/andres-nav/minion/internal/config"
	"github.com/andres-nav/minion/internal/remote"
)

// cmdRun runs a task by name (the first positional argument), resolved from the
// template's standard verbs (build/test/lint/clean) or its freeform tasks map.
// It runs on the resolved host (template/repo host or --host); use
// --host localhost to run on this machine.
func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	host := hostFlag(fs)
	detach := fs.Bool("detach", false, "launch without attaching")
	arg := fs.String("arg", "", "task text exposed to the command/hooks as $MINION_ARGS")
	noSync := fs.Bool("no-sync", false, "don't sync local files; run against the code already on the host")
	taskName, err := parsePositional(fs, args)
	if err != nil {
		return 2
	}
	if taskName == "" {
		fmt.Fprintln(os.Stderr, "usage: minion run <task>")
		return 2
	}

	cfg, r, err := resolveContext()
	if err != nil {
		return errf("%v", err)
	}
	if r.TemplateName == "" {
		fmt.Fprintf(os.Stderr, "minion: no template for %s (add detect rules or a .minion.json with \"use\")\n", r.Root)
		return 1
	}
	command, ok := r.Template.Lookup(taskName)
	if !ok {
		fmt.Fprintf(os.Stderr, "minion: template %q has no task %q\n", r.TemplateName, taskName)
		if names := taskNames(r.Template); names != "" {
			fmt.Fprintf(os.Stderr, "available tasks: %s\n", names)
		}
		return 1
	}

	job := remote.Job{
		Dir:       r.Root,
		Env:       r.Template.Env,
		Command:   command,
		Args:      *arg,
		Before:    r.Template.Before,
		OnSuccess: r.Template.OnSuccess,
		OnFailure: r.Template.OnFailure,
		Always:    r.Template.Always,
	}

	// Validate the effective notify config before touching the network, so a typo
	// (e.g. on: "faliure") fails loudly here instead of silently notifying always.
	notif := cfg.NotifyFor(r.Template)
	if err := notif.Validate(); err != nil {
		return errf("notify config: %v", err)
	}

	rt, err := dialRemote(cfg, r, *host)
	if err != nil {
		return errf("%v", err)
	}
	defer rt.close()
	return runRemote(rt, taskName, job, notif, *detach, *noSync)
}

func runRemote(rt *remoteTarget, taskName string, job remote.Job, notif *config.Notify, detach, noSync bool) int {
	r := rt.resolved

	// Local runs happen in place; remote runs sync the project (and any sibling
	// dependency packages) up first. Dependency build order is Brazil's job
	// (brazil-recursive-cmd) — minion just gets the edited sources there. --no-sync
	// skips this to run against code already on the host (e.g. a mainline checkout).
	if !rt.local && !noSync {
		// Push local dirs (e.g. ~/.midway) to the same remote path first, so the
		// remote has fresh credentials before the build runs.
		for _, p := range r.Template.Push {
			pn, _, perr := pushChanges(rt.exec, rt.invoke, expandLocalHome(p), nil, p, false)
			if perr != nil {
				return errf("push %s: %v", p, perr)
			}
			fmt.Fprintf(os.Stderr, "pushed %s (%d file(s))\n", p, pn)
		}
		for _, need := range r.Template.Needs {
			needLocal := filepath.Join(filepath.Dir(r.Root), need)
			needRemote := remoteDirFor(r.Template.RemoteDir, rt.host.RemoteDir, needLocal)
			dn, _, derr := pushChanges(rt.exec, rt.invoke, needLocal, r.Template.Exclude, needRemote, false)
			if derr != nil {
				return errf("dependency %s: %v", need, derr)
			}
			fmt.Fprintf(os.Stderr, "synced dep %s (%d file(s)) to %s:%s\n", need, dn, rt.alias, needRemote)
		}
		n, _, err := pushChanges(rt.exec, rt.invoke, r.Root, r.Template.Exclude, rt.dir, false)
		if err != nil {
			return errf("%v", err)
		}
		fmt.Fprintf(os.Stderr, "synced %d file(s) to %s:%s\n", n, rt.alias, rt.dir)
	}

	id := randomName()
	ts := time.Now().Format(tsLayout)
	jobDir := remote.JobDir(id)
	job.Dir = rt.dir
	if notif != nil {
		job.Notify = buildNotify(notifyInvoke(rt), jobDir+"/log", notif)
	}
	sha, dirty := gitProvenance(r.Root)
	meta := remote.Meta{ID: id, Task: taskName, Host: rt.alias, Git: sha, Dirty: dirty}
	script := remote.Script(rt.dir, jobDir, job, meta)

	if code, err := rt.exec.Run(remote.LaunchCommand(jobDir), strings.NewReader(script), io.Discard, os.Stderr); err != nil || code != 0 {
		return errf("launch failed (err=%v exit=%d)", err, code)
	}
	fmt.Fprintf(os.Stderr, "started %s on %s\n", id, rt.alias)

	if detach {
		fmt.Println(id)
		return 0
	}

	code, err := rt.exec.Run(remote.AttachCommand(jobDir), nil, os.Stdout, os.Stderr)
	if err != nil {
		return errf("detached (job %s still running): %v", id, err)
	}

	// Fetch results (remote only — local runs already wrote them in place),
	// regardless of exit code, since failed runs still leave useful output.
	if !rt.local && len(r.Template.Fetch) > 0 {
		dest := fetchDest(r.Root, r.Template.FetchInto, id, ts, filepath.Base(r.Root))
		if n, ferr := pullResults(rt.exec, rt.invoke, rt.dir, r.Template.Fetch, dest); ferr != nil {
			fmt.Fprintf(os.Stderr, "minion: fetch failed: %v\n", ferr)
		} else {
			fmt.Fprintf(os.Stderr, "fetched %d file(s) to %s\n", n, dest)
		}
	}
	return code
}

func taskNames(t config.Template) string {
	var names []string
	for _, task := range t.OrderedTasks() {
		names = append(names, task.Name)
	}
	return strings.Join(names, ", ")
}

// gitProvenance returns the commit SHA and dirty flag of the tree at dir.
func gitProvenance(dir string) (sha string, dirty bool) {
	sha = strings.TrimSpace(gitOut(dir, "rev-parse", "HEAD"))
	dirty = strings.TrimSpace(gitOut(dir, "status", "--porcelain")) != ""
	return
}

func gitOut(dir string, args ...string) string {
	out, _ := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	return string(out)
}

// --- job notify wiring -------------------------------------------------------
// How a job announces itself when it finishes: buildNotify emits the sh snippet
// appended to the job script; emailInvoke/gate/resolveSecret build the pieces.
// Shared by the run path and `doctor --notify`.

// buildNotify assembles the shell run at the end of a job's script. It resolves
// ${ENV} secrets on this machine (so credentials never touch the config file or
// the repo), gates on the configured `on`, invokes the agent for the email sink,
// and appends any raw `run` commands. invoke is the minion binary to call on the
// target; logPath is the job's log file (passed to the agent for a body tail).
func buildNotify(invoke, logPath string, n *config.Notify) string {
	if n.Email == nil && len(n.Run) == 0 {
		return ""
	}
	var b strings.Builder
	if n.Email != nil {
		if pass := resolveSecret(n.Email.Pass); pass != "" {
			fmt.Fprintf(&b, "export MINION_SMTP_PASS=%s\n", remote.ShellQuote(pass))
		}
	}
	fmt.Fprintf(&b, "if %s; then\n", gate(n.On))
	if n.Email != nil {
		fmt.Fprintf(&b, "  %s || echo '[minion] notify: email failed' >&2\n", emailInvoke(invoke, logPath, n.Email))
	}
	for _, c := range n.Run {
		fmt.Fprintf(&b, "  ( %s ) || echo '[minion] notify: run hook failed' >&2\n", c)
	}
	b.WriteString("fi")
	return b.String()
}

// emailInvoke is the agent call that sends the email. Non-secret fields are
// flags; the password is read from MINION_SMTP_PASS in the environment.
func emailInvoke(invoke, logPath string, e *config.EmailSink) string {
	parts := []string{invoke, "_agent", "notify",
		"--smtp", remote.ShellQuote(e.SMTP),
		"--from", remote.ShellQuote(e.From),
		"--to", remote.ShellQuote(e.To),
	}
	if e.User != "" {
		parts = append(parts, "--user", remote.ShellQuote(e.User))
	}
	if logPath != "" {
		parts = append(parts, "--log", logPath) // unquoted so the remote shell expands ~
	}
	return strings.Join(parts, " ")
}

// gate is the test expression deciding whether to notify, based on `on`.
func gate(on string) string {
	switch on {
	case "failure":
		return `[ "$MINION_EXIT" != 0 ]`
	case "success":
		return `[ "$MINION_EXIT" = 0 ]`
	default: // "always" or unset
		return "true"
	}
}

// resolveSecret expands a ${VAR} or $VAR reference from this machine's
// environment; any other value is returned literally.
func resolveSecret(s string) string {
	if strings.HasPrefix(s, "${") && strings.HasSuffix(s, "}") {
		return os.Getenv(s[2 : len(s)-1])
	}
	if strings.HasPrefix(s, "$") {
		return os.Getenv(s[1:])
	}
	return s
}

// notifyInvoke returns the minion binary to call on the target: the uploaded
// agent for a remote, or this executable for a local run.
func notifyInvoke(rt *remoteTarget) string {
	if !rt.local && rt.invoke != "" {
		return rt.invoke
	}
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "minion"
}
