package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/andres-nav/minion/minion/internal/config"
	"github.com/andres-nav/minion/minion/internal/remote"
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

	ts := stamp(time.Now())
	job.Dir = rt.dir
	job.Agent = notifyInvoke(rt)
	if notif != nil {
		job.Notify = resolvedNotify(notif)
	}
	sha, dirty := gitProvenance(r.Root)
	id, err := launch(rt.exec, rt.state, ts, func(id, jobDir string) string {
		meta := remote.Meta{ID: id, Task: taskName, Host: rt.alias, Git: sha, Dirty: dirty}
		return remote.Script(rt.dir, jobDir, job, meta)
	})
	if err != nil {
		return errf("%v", err)
	}
	jobDir := remote.JobDir(rt.state, id)
	fmt.Fprintf(os.Stderr, "started %s on %s\n", id, rt.alias)

	if detach {
		fmt.Println(id)
		return 0
	}

	code, err := rt.exec.Run(remote.AttachCommand(notifyInvoke(rt), jobDir), nil, os.Stdout, os.Stderr)
	if err != nil {
		return errf("detached (job %s still running): %v", id, err)
	}

	// Fetch results (remote only — local runs already wrote them in place),
	// regardless of exit code, since failed runs still leave useful output.
	if !rt.local && len(r.Template.Fetch) > 0 {
		dest := fetchDest(r.Root, r.Template.FetchInto, id, ts, filepath.Base(r.Root))
		if n, ferr := pullResults(rt.exec, rt.invoke, remote.RemotePath(rt.dir), r.Template.Fetch, dest); ferr != nil {
			fmt.Fprintf(os.Stderr, "minion: fetch failed: %v\n", ferr)
		} else {
			fmt.Fprintf(os.Stderr, "fetched %d file(s) to %s\n", n, dest)
		}
		pullJob(rt, jobDir, dest)
	}
	return code
}

// launch starts a job under a new id (a random name and ts), drawing another
// name while the id is taken, so a job never reuses an old job's files.
func launch(ex execer, state, ts string, script func(id, jobDir string) string) (string, error) {
	for range 5 {
		id := randomName() + "-" + ts
		s := script(id, remote.JobDir(state, id))
		code, err := ex.Run(remote.LaunchCommand(state, id), strings.NewReader(s), io.Discard, os.Stderr)
		switch {
		case err == nil && code == 0:
			return id, nil
		case err != nil || code != remote.IDTaken:
			return "", fmt.Errorf("launch failed (err=%v exit=%d)", err, code)
		}
	}
	return "", errors.New("launch failed: no free job id")
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
// A job announces itself through the agent on the target, which reads the
// resolved notify block from a file in the job dir. Shared by the run path and
// `doctor --notify`.

// resolvedNotify returns n as one line of JSON with its ${ENV} secrets
// resolved on this machine, so credentials never live in the config or repo.
func resolvedNotify(n *config.Notify) string {
	r := *n
	if n.Email != nil {
		e := *n.Email
		e.Pass = resolveSecret(e.Pass)
		r.Email = &e
	}
	r.Env = map[string]string{}
	for k, v := range n.Env {
		r.Env[k] = resolveSecret(v)
	}
	b, _ := json.Marshal(r)
	return string(b)
}

// sortedKeys returns the map keys in a stable order.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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

// notifyInvoke returns the minion binary to call on the target, as one shell
// word: the uploaded agent for a remote, or this executable for a local run.
func notifyInvoke(rt *remoteTarget) string {
	if !rt.local && rt.invoke != "" {
		return rt.invoke
	}
	if exe, err := os.Executable(); err == nil {
		return remote.ShellQuote(exe)
	}
	return "minion"
}
