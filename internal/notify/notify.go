// Package notify sends a "job finished" notification from the machine the job
// ran on. Transports use only the Go standard library (net/smtp), so the remote
// needs nothing beyond the minion agent already uploaded there — no curl, no
// mail client, no credentials daemon.
package notify

import (
	"fmt"
	"net/smtp"
	"strconv"
	"strings"
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

// ok reports whether the job succeeded.
func (r Result) ok() bool { return r.Exit == 0 }

// Subject is the one-line summary (also used as the email subject).
func (r Result) Subject() string {
	state := "FAILED"
	if r.ok() {
		state = "ok"
	}
	return fmt.Sprintf("minion: %s on %s %s (exit %d, %s)",
		r.Task, r.Host, state, r.Exit, humanDuration(r.Duration))
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

func humanDuration(sec int) string {
	if sec < 60 {
		return strconv.Itoa(sec) + "s"
	}
	return fmt.Sprintf("%dm%02ds", sec/60, sec%60)
}

// Email is an SMTP transport. Addr is host:port (e.g. smtp.gmail.com:587). Auth
// is attempted when User is set; net/smtp negotiates STARTTLS when the server
// advertises it (as Gmail does on :587).
type Email struct {
	Addr string
	From string
	To   []string
	User string // SMTP login; defaults to From when empty
	Pass string
}

// Send delivers subject/body to every recipient in one message.
func (e Email) Send(subject, body string) error {
	if e.Addr == "" || e.From == "" || len(e.To) == 0 {
		return fmt.Errorf("email: addr, from, and to are required")
	}
	host, _, err := splitHostPort(e.Addr)
	if err != nil {
		return err
	}
	var auth smtp.Auth
	if user := e.login(); user != "" {
		auth = smtp.PlainAuth("", user, e.Pass, host)
	}
	msg := buildMessage(e.From, e.To, subject, body)
	if err := smtp.SendMail(e.Addr, auth, e.From, e.To, msg); err != nil {
		return fmt.Errorf("email: send via %s: %w", e.Addr, err)
	}
	return nil
}

func (e Email) login() string {
	if e.User != "" {
		return e.User
	}
	if e.Pass != "" {
		return e.From // password given but no explicit user: log in as the sender
	}
	return ""
}

// buildMessage assembles an RFC 5322 message with CRLF line endings.
func buildMessage(from string, to []string, subject, body string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("\r\n")
	// normalize body to CRLF and dot-stuff so a lone "." can't end the DATA phase.
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, ".") {
			line = "." + line
		}
		b.WriteString(line)
		b.WriteString("\r\n")
	}
	return []byte(b.String())
}

// splitHostPort splits host:port, requiring both parts (SMTP needs an explicit port).
func splitHostPort(addr string) (host, port string, err error) {
	host, port, ok := strings.Cut(addr, ":")
	if !ok || host == "" || port == "" {
		return "", "", fmt.Errorf("email: smtp address %q must be host:port", addr)
	}
	return host, port, nil
}
