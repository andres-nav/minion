// Package notify composes a finished job's message, which the notify run hooks
// get as $GRU_SUBJECT and $GRU_BODY.
package notify

import (
	"fmt"
	"strings"
	"time"
)

// Result is the outcome of a finished job, used to compose the message.
type Result struct {
	JobID    string
	Task     string
	Host     string
	Exit     int
	Duration int // seconds
	LogTail  string
}

// Subject is the one-line summary.
func (r Result) Subject() string {
	state := "FAILED"
	if r.Exit == 0 {
		state = "ok"
	}
	return fmt.Sprintf("gru: %s on %s %s (exit %d, %s)",
		r.Task, r.Host, state, r.Exit, time.Duration(r.Duration)*time.Second)
}

// Body is the full message: the summary line, provenance, and a log tail.
func (r Result) Body() string {
	var b strings.Builder
	b.WriteString(r.Subject())
	fmt.Fprintf(&b, "\n\njob:  %s\ntask: %s\nhost: %s\nexit: %d\n", r.JobID, r.Task, r.Host, r.Exit)
	if tail := strings.TrimRight(r.LogTail, "\n"); tail != "" {
		b.WriteString("\n--- log tail ---\n")
		b.WriteString(tail)
		b.WriteString("\n")
	}
	return b.String()
}
