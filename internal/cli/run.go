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

// cmdTask runs a task. verb is a standard verb ("build", "test", …); for "run"
// the task name is the first positional argument. It runs on the resolved host
// (template/repo host or --host); use --host localhost to run on this machine.
func cmdTask(verb string, args []string) int {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	host := fs.String("host", "", "host to run on (a hosts-map name, ssh alias, or localhost)")
	detach := fs.Bool("detach", false, "launch without attaching")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	taskName := verb
	if verb == "run" {
		taskName = fs.Arg(0)
		if taskName == "" {
			fmt.Fprintln(os.Stderr, "usage: minion run <task>")
			return 2
		}
		if err := reparseTrailingFlags(fs); err != nil {
			return 2
		}
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
		Before:    r.Template.Before,
		OnSuccess: r.Template.OnSuccess,
		OnFailure: r.Template.OnFailure,
		Always:    r.Template.Always,
	}

	rt, err := dialRemote(cfg, r, *host)
	if err != nil {
		return errf("%v", err)
	}
	return runRemote(rt, taskName, job, *detach)
}

func runRemote(rt *remoteTarget, taskName string, job remote.Job, detach bool) int {
	r := rt.resolved

	// Local runs happen in place; remote runs sync the project (and any sibling
	// dependency packages) up first. Dependency build order is Brazil's job
	// (brazil-recursive-cmd) — minion just gets the edited sources there.
	if !rt.local {
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
