package cli

import (
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/andres-nav/minion/internal/remote"
)

func cmdPs(args []string) int {
	fs := flag.NewFlagSet("ps", flag.ContinueOnError)
	host := fs.String("host", "", "host")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	client, alias, err := executorFor(*host)
	if err != nil {
		return errf("%v", err)
	}
	out, err := client.Output(remote.PsCommand())
	if err != nil {
		return errf("%v", err)
	}
	if strings.TrimSpace(out) == "" {
		fmt.Printf("no jobs on %s\n", alias)
		return 0
	}

	fmt.Printf("%-18s %-10s %-17s %-6s %s\n", "NAME", "TASK", "STARTED", "AGE", "STATUS")
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		f := strings.SplitN(line, "\t", 4)
		if len(f) != 4 {
			continue
		}
		id, task, started, status := f[0], f[1], f[2], f[3]
		when, age := "?", "?"
		if epoch, err := strconv.ParseInt(started, 10, 64); err == nil {
			t := time.Unix(epoch, 0).Local()
			when = t.Format("2006-01-02 15:04")
			age = humanizeAge(time.Since(t))
		}
		fmt.Printf("%-18s %-10s %-17s %-6s %s\n", id, task, when, age, status)
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
