package cli

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/andres-nav/minion/internal/remote"
	"github.com/andres-nav/minion/internal/ssh"
)

func cmdPs(args []string) int {
	fs := flag.NewFlagSet("ps", flag.ContinueOnError)
	host := fs.String("host", "", "host")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	alias, err := aliasFor(*host)
	if err != nil {
		return errf("%v", err)
	}
	out, err := ssh.New(alias).Output(remote.PsCommand())
	if err != nil {
		return errf("%v", err)
	}
	if strings.TrimSpace(out) == "" {
		fmt.Printf("no jobs on %s\n", alias)
		return 0
	}

	fmt.Printf("%-24s %-12s %-6s %s\n", "JOB", "TASK", "AGE", "STATUS")
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		id, status := parts[0], parts[1]
		task, t, ok := parseJobID(id)
		age := "?"
		if ok {
			age = humanizeAge(time.Since(t))
		}
		fmt.Printf("%-24s %-12s %-6s %s\n", id, task, age, status)
	}
	return 0
}

// parseJobID splits "<task>-YYYYmmdd-HHMMSS" into its task and start time.
func parseJobID(id string) (task string, t time.Time, ok bool) {
	i := strings.LastIndex(id, "-")
	if i <= 0 {
		return "", time.Time{}, false
	}
	j := strings.LastIndex(id[:i], "-")
	if j <= 0 {
		return "", time.Time{}, false
	}
	parsed, err := time.ParseInLocation("20060102-150405", id[j+1:], time.Local)
	if err != nil {
		return "", time.Time{}, false
	}
	return id[:j], parsed, true
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
