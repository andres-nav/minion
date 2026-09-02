package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/andres-nav/minion/internal/config"
	"github.com/andres-nav/minion/internal/remote"
	"github.com/andres-nav/minion/internal/runner"
)

// cmdTask runs a task. verb is a standard verb ("build", "test", …); for "run"
// the task name is the first positional argument. It runs remotely when a host
// is resolved (template/repo host or --host), else locally; --local forces local.
func cmdTask(verb string, args []string) int {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	host := fs.String("host", "", "run on a remote host")
	local := fs.Bool("local", false, "force local execution")
	var detach bool
	fs.BoolVar(&detach, "detach", false, "launch remotely without attaching")
	fs.BoolVar(&detach, "d", false, "shorthand for --detach")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	taskName := verb
	if verb == "run" {
		if fs.NArg() == 0 {
			fmt.Fprintln(os.Stderr, "usage: minion run <task>")
			return 2
		}
		taskName = fs.Arg(0)
		if fs.NArg() > 1 { // allow flags after the task name
			if err := fs.Parse(fs.Args()[1:]); err != nil {
				return 2
			}
		}
	}

	cfg, r, err := resolveContext()
	if err != nil {
		fmt.Fprintf(os.Stderr, "minion: %v\n", err)
		return 1
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

	job := runner.Job{
		Dir:       r.Root,
		Env:       r.Template.Env,
		Command:   command,
		Before:    r.Template.Before,
		OnSuccess: r.Template.OnSuccess,
		OnFailure: r.Template.OnFailure,
		Always:    r.Template.Always,
	}

	if *local {
		return runner.Local(job, os.Stdout, os.Stderr)
	}
	rt, err := dialRemote(cfg, r, *host)
	if err != nil {
		if errors.Is(err, errNoHost) {
			return runner.Local(job, os.Stdout, os.Stderr)
		}
		return errf("%v", err)
	}
	return runRemote(rt, taskName, job, detach)
}

func runRemote(rt *remoteTarget, taskName string, job runner.Job, detach bool) int {
	r := rt.resolved

	n, _, err := pushChanges(rt.client, rt.invoke, r.Root, r.Template.Exclude, rt.dir, false)
	if err != nil {
		return errf("%v", err)
	}
	fmt.Fprintf(os.Stderr, "synced %d file(s) to %s:%s\n", n, rt.alias, rt.dir)

	id := taskName + "-" + time.Now().Format("20060102-150405")
	jobDir := remote.JobDir(id)
	job.Dir = rt.dir
	sha, dirty := gitProvenance(r.Root)
	meta := remote.Meta{ID: id, Task: taskName, Host: rt.alias, Git: sha, Dirty: dirty}
	script := remote.Script(rt.dir, jobDir, job, meta)

	if code, err := rt.client.Run(remote.LaunchCommand(jobDir), strings.NewReader(script), io.Discard, os.Stderr); err != nil || code != 0 {
		return errf("launch failed (err=%v exit=%d)", err, code)
	}
	fmt.Fprintf(os.Stderr, "started %s on %s\n", id, rt.alias)

	if detach {
		fmt.Println(id)
		return 0
	}

	code, err := rt.client.Run(remote.AttachCommand(jobDir), nil, os.Stdout, os.Stderr)
	if err != nil {
		return errf("detached (job %s still running): %v", id, err)
	}

	// Fetch results regardless of exit code — failed runs still leave useful
	// output/logs worth pulling back.
	if len(r.Template.Fetch) > 0 {
		if n, ferr := pullResults(rt.client, rt.invoke, rt.dir, r.Template.Fetch, r.Root); ferr != nil {
			fmt.Fprintf(os.Stderr, "minion: fetch failed: %v\n", ferr)
		} else {
			fmt.Fprintf(os.Stderr, "fetched %d file(s) to %s\n", n, r.Root)
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
