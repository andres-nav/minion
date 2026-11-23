package cli

import (
	"flag"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/andres-nav/minion/gru/internal/remote"
)

type job struct {
	id, status, repo, task string
	start                  time.Time
}

// jobs lists t's jobs of this host, newest first; with all, of every project.
func jobs(t *target, all bool) ([]job, error) {
	out, err := t.Output(remote.PsCommand(t.state))
	if err != nil {
		return nil, err
	}
	var js []job
	for line := range strings.Lines(out) {
		f := strings.SplitN(strings.TrimRight(line, "\n"), "\t", 4)
		if len(f) != 4 || (!all && f[2] != filepath.Base(t.r.Root)) {
			continue
		}
		start, _ := idTime(f[0])
		js = append(js, job{id: f[0], status: f[1], repo: f[2], task: f[3], start: start})
	}
	sort.SliceStable(js, func(i, j int) bool { return js[i].start.After(js[j].start) })
	return js, nil
}

// jobID checks a given job id, or returns the latest job of this project.
func jobID(t *target, id string) (string, error) {
	if id != "" {
		return id, remote.CheckID(id)
	}
	js, err := jobs(t, false)
	if err != nil || len(js) == 0 {
		return "", err
	}
	return js[0].id, nil
}

func cmdPs(args []string) int {
	fs := flag.NewFlagSet("ps", flag.ContinueOnError)
	host := hostFlag(fs)
	all := fs.Bool("all", false, "show all jobs on the host, not just this project's")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	r, err := project()
	if err != nil {
		return errf("%v", err)
	}
	t, err := dial(r, *host, false)
	if err != nil {
		return errf("%v", err)
	}
	js, err := jobs(t, *all)
	if err != nil {
		return errf("%v", err)
	}
	fmt.Printf("%-32s %-10s %-17s %-6s %s\n", "NAME", "TASK", "STARTED", "AGE", "STATUS")
	for _, j := range js {
		when, age := "?", "?"
		if !j.start.IsZero() {
			when, age = psTime(j.start), humanizeAge(time.Since(j.start))
		}
		fmt.Printf("%-32s %-10s %-17s %-6s %s\n", j.id, j.task, when, age, j.status)
	}
	if len(js) == 0 && *all {
		fmt.Printf("(no jobs on %s)\n", t.alias)
	} else if len(js) == 0 {
		fmt.Printf("(no jobs for %q on %s — use --all)\n", filepath.Base(r.Root), t.alias)
	}
	return 0
}

// psTime shows a job's start time, in UTC.
func psTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04Z") }

func humanizeAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(int(d.Seconds()), 0))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
