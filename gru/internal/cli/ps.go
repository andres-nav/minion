package cli

import (
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/andres-nav/minion/gru/internal/remote"
)

type job struct {
	id, status, task string
	start            time.Time
}

// jobs lists t's jobs of this host, newest launch first; with all, of every project.
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
		js = append(js, job{id: f[0], status: f[1], task: f[3], start: start})
	}
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
	if _, err := parseArgs(fs, args, false); err != nil {
		return 2
	}
	t, err := connect(*host, false)
	if err != nil {
		return errf("%v", err)
	}
	js, err := jobs(t, *all)
	if err != nil {
		return errf("%v", err)
	}
	fmt.Printf("%-32s %-10s %-17s %s\n", "NAME", "TASK", "STARTED", "STATUS")
	for _, j := range js {
		when := "?"
		if !j.start.IsZero() {
			when = psTime(j.start)
		}
		fmt.Printf("%-32s %-10s %-17s %s\n", j.id, j.task, when, j.status)
	}
	if len(js) == 0 && *all {
		fmt.Printf("(no jobs on %s)\n", t.alias)
	} else if len(js) == 0 {
		fmt.Printf("(no jobs for %q on %s — use --all)\n", filepath.Base(t.r.Root), t.alias)
	}
	return 0
}

// psTime shows a job's start time, in UTC.
func psTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04Z") }
