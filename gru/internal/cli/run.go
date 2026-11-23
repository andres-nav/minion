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
	"strings"
	"time"

	"github.com/andres-nav/minion/gru/internal/config"
	"github.com/andres-nav/minion/gru/internal/remote"
)

// cmdRun runs a task of the template on the resolved host (the template's or
// --host; --host localhost runs it here): sync, launch, attach, fetch.
func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	host := hostFlag(fs)
	detach := fs.Bool("detach", false, "launch without attaching")
	arg := fs.String("arg", "", "task text exposed to the command/hooks as $GRU_ARGS")
	noSync := fs.Bool("no-sync", false, "don't sync local files; run against the code already on the host")
	task, err := parsePositional(fs, args)
	if err != nil {
		return 2
	}
	if task == "" {
		fmt.Fprintln(os.Stderr, "usage: gru run <task>")
		return 2
	}

	r, err := project()
	if err != nil {
		return errf("%v", err)
	}
	tm := r.Template
	command, ok := tm.Tasks[task]
	switch {
	case len(tm.Tasks) == 0:
		return errf("no tasks for %s (add detect rules to the config, or a .gru.json)", r.Root)
	case !ok:
		return errf("no task %q for %s (tasks: %s)", task, r.Root, strings.Join(tm.TaskNames(), ", "))
	}
	notify, err := resolvedNotify(r.Config.NotifyFor(tm))
	if err != nil {
		return errf("notify: %v", err)
	}
	t, err := dial(r, *host, true)
	if err != nil {
		return errf("%v", err)
	}
	defer t.Close()

	if !t.Local && !*noSync {
		// Push local dirs (e.g. ~/.midway) to the same remote path, then the
		// sibling packages the project needs, then the project.
		for _, p := range tm.Push {
			n, _, err := pushChanges(t, expandLocalHome(p), nil, p, false, false)
			if err != nil {
				return errf("push %s: %v", p, err)
			}
			fmt.Fprintf(os.Stderr, "pushed %s (%d file(s))\n", p, n)
		}
		for _, need := range tm.Needs {
			local := filepath.Join(filepath.Dir(r.Root), need)
			dir := remoteDirFor(tm.RemoteDir, t.host.RemoteDir, local)
			if dir == t.dir {
				return errf("needs %s: it would sync into the project's remote dir %s (put {repo} in remote_dir)", need, dir)
			}
			n, _, err := pushChanges(t, local, tm.Exclude, dir, false, true)
			if err != nil {
				return errf("needs %s: %v", need, err)
			}
			fmt.Fprintf(os.Stderr, "synced dep %s (%d file(s)) to %s:%s\n", need, n, t.alias, dir)
		}
		n, _, err := pushChanges(t, r.Root, tm.Exclude, t.dir, false, true)
		if err != nil {
			return errf("%v", err)
		}
		fmt.Fprintf(os.Stderr, "synced %d file(s) to %s:%s\n", n, t.alias, t.dir)
	}

	sha, dirty := gitProvenance(r.Root)
	job := remote.Job{Task: task, Host: t.alias, Git: sha, Dirty: dirty, Env: tm.Env, Command: command, Args: *arg,
		Before: tm.Before, OnSuccess: tm.OnSuccess, OnFailure: tm.OnFailure, Always: tm.Always, Notify: notify}
	id, code := start(t, job, *detach)
	// Fetch results (remote only — local runs already wrote them in place),
	// whatever the exit code: failed runs still leave useful output.
	if id != "" && !t.Local && len(tm.Fetch) > 0 {
		dest := fetchDest(r.Root, tm.FetchInto, id, filepath.Base(r.Root))
		if n, err := pullResults(t, remote.RemotePath(t.dir), tm.Fetch, dest); err != nil {
			fmt.Fprintf(os.Stderr, "gru: fetch failed: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "fetched %d file(s) to %s\n", n, dest)
		}
		pullJob(t, id, dest)
	}
	return code
}

// start launches job on t and, unless detach, follows it to its end. It
// returns the job's id once it ended ("" if detached or still running) and
// the exit code.
func start(t *target, job remote.Job, detach bool) (string, int) {
	job.Repo, job.Agent = filepath.Base(t.r.Root), t.agent
	id, err := launch(t, stamp(time.Now()), func(id string) string {
		job.ID = id
		return remote.Script(t.dir, remote.JobDir(t.state, id), job)
	})
	if err != nil {
		return "", errf("%v", err)
	}
	fmt.Fprintf(os.Stderr, "started %s on %s\n", id, t.alias)
	if detach {
		fmt.Println(id)
		return "", 0
	}
	jobDir := remote.JobDir(t.state, id)
	code, err := t.Run(remote.AttachCommand(t.agent, jobDir), nil, os.Stdout, os.Stderr)
	if err != nil || code == 255 {
		// ssh failed: the job may still run, without us.
		if _, serr := t.Output("test -e " + jobDir + "/status"); serr != nil {
			return "", errf("lost %s; job %s may still run (gru attach %s)", t.alias, id, id)
		}
	}
	return id, code
}

// launch starts a job under a new id (a random name and ts), drawing another
// name while the id is taken, so a job never reuses an old job's files.
func launch(t *target, ts string, script func(id string) string) (string, error) {
	for range 5 {
		id := randomName() + "-" + ts
		code, err := t.Run(remote.LaunchCommand(t.state, id), strings.NewReader(script(id)), io.Discard, os.Stderr)
		switch {
		case err == nil && code == 0:
			return id, nil
		case err != nil || code != remote.IDTaken:
			return "", fmt.Errorf("launch failed (err=%v exit=%d)", err, code)
		}
	}
	return "", errors.New("launch failed: no free job id")
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

// resolvedNotify returns n as one line of JSON with its ${ENV} secrets
// resolved on this machine, so credentials never live in the config or repo.
// It is "" without a notify block.
func resolvedNotify(n *config.Notify) (string, error) {
	if n == nil {
		return "", nil
	}
	r := *n
	var err error
	if n.Email != nil {
		e := *n.Email
		if e.Pass, err = resolveSecret(e.Pass); err != nil {
			return "", err
		}
		r.Email = &e
	}
	r.Env = map[string]string{}
	for k, v := range n.Env {
		if r.Env[k], err = resolveSecret(v); err != nil {
			return "", err
		}
	}
	b, _ := json.Marshal(r)
	return string(b), nil
}

// resolveSecret expands a ${VAR} or $VAR reference from this machine's
// environment, refusing an unset variable; any other value is returned literally.
func resolveSecret(s string) (string, error) {
	name, ok := strings.CutPrefix(s, "$")
	if !ok {
		return s, nil
	}
	name = strings.TrimSuffix(strings.TrimPrefix(name, "{"), "}")
	v, ok := os.LookupEnv(name)
	if !ok {
		return "", fmt.Errorf("$%s is not set", name)
	}
	return v, nil
}
