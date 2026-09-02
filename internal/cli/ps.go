package cli

import (
	"flag"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/andres-nav/minion/internal/remote"
)

type psRow struct {
	id, task, when, age, status string
	epoch                       int64
}

func cmdPs(args []string) int {
	fs := flag.NewFlagSet("ps", flag.ContinueOnError)
	host := fs.String("host", "", "host")
	all := fs.Bool("all", false, "show all jobs on the host, not just this package's")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	client, alias, err := executorFor(*host)
	if err != nil {
		return errf("%v", err)
	}

	// Default: only this package's jobs (matched by dir name).
	cur := ""
	if _, r, cerr := resolveContext(); cerr == nil {
		cur = filepath.Base(r.Root)
	}

	out, err := client.Output(remote.PsCommand())
	if err != nil {
		return errf("%v", err)
	}

	var rows []psRow
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		f := strings.SplitN(line, "\t", 5)
		if len(f) != 5 {
			continue
		}
		id, task, started, status, repo := f[0], f[1], f[2], f[3], f[4]
		if !*all && cur != "" && repo != cur {
			continue
		}
		row := psRow{id: id, task: task, when: "?", age: "?", status: status}
		if epoch, perr := strconv.ParseInt(started, 10, 64); perr == nil {
			t := time.Unix(epoch, 0).Local()
			row.epoch = epoch
			row.when = t.Format("2006-01-02 15:04")
			row.age = humanizeAge(time.Since(t))
		}
		rows = append(rows, row)
	}
	// most recent first
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].epoch > rows[j].epoch })

	fmt.Printf("%-18s %-10s %-17s %-6s %s\n", "NAME", "TASK", "STARTED", "AGE", "STATUS")
	for _, row := range rows {
		fmt.Printf("%-18s %-10s %-17s %-6s %s\n", row.id, row.task, row.when, row.age, row.status)
	}
	if len(rows) == 0 {
		if *all {
			fmt.Printf("(no jobs on %s)\n", alias)
		} else {
			fmt.Printf("(no jobs for %q on %s — use --all)\n", cur, alias)
		}
	}
	return 0
}

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
