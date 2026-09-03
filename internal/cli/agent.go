package cli

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/andres-nav/minion/internal/notify"
	"github.com/andres-nav/minion/internal/sync"
)

// cmdAgent is the hidden remote-side counterpart invoked over ssh.
func cmdAgent(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "minion _agent: missing subcommand")
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Printf("minion %s\n", version)
		return 0
	case "manifest":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "minion _agent manifest: missing dir")
			return 2
		}
		if err := sync.WriteManifest(os.Stdout, args[1], args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "minion _agent: %v\n", err)
			return 1
		}
		return 0
	case "recv":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "minion _agent recv: missing dir")
			return 2
		}
		n, err := sync.Receive(os.Stdin, args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "minion _agent: %v\n", err)
			return 1
		}
		fmt.Printf("%d\n", n)
		return 0
	case "send":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "minion _agent send: missing dir")
			return 2
		}
		paths, err := sync.Match(args[1], args[2:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "minion _agent: %v\n", err)
			return 1
		}
		if err := sync.Send(os.Stdout, args[1], paths, nil); err != nil {
			fmt.Fprintf(os.Stderr, "minion _agent: %v\n", err)
			return 1
		}
		return 0
	case "notify":
		return agentNotify(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "minion _agent: unknown subcommand %q\n", args[0])
		return 2
	}
}

// agentNotify sends a job-finished notification. Transport config comes from
// flags (non-secret) and MINION_SMTP_PASS in the environment (secret); the job
// outcome comes from the MINION_* env the run wrapper exported.
func agentNotify(args []string) int {
	fs := flag.NewFlagSet("notify", flag.ContinueOnError)
	smtpAddr := fs.String("smtp", "", "SMTP server as host:port")
	from := fs.String("from", "", "sender address")
	to := fs.String("to", "", "recipient address(es), comma-separated")
	user := fs.String("user", "", "SMTP login (defaults to --from)")
	logPath := fs.String("log", "", "job log file, tailed into the message body")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	res := notify.Result{
		JobID:    os.Getenv("MINION_JOB_ID"),
		Task:     os.Getenv("MINION_TASK"),
		Host:     os.Getenv("MINION_HOST"),
		Exit:     atoiOr(os.Getenv("MINION_EXIT"), -1),
		Duration: atoiOr(os.Getenv("MINION_DURATION"), 0),
		LogTail:  tailFile(*logPath, 30),
	}

	sink := notify.Email{
		Addr: *smtpAddr,
		From: *from,
		To:   splitList(*to),
		User: *user,
		Pass: os.Getenv("MINION_SMTP_PASS"),
	}
	if err := sink.Send(res.Subject(), res.Body()); err != nil {
		fmt.Fprintf(os.Stderr, "minion _agent notify: %v\n", err)
		return 1
	}
	return 0
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// tailFile returns the last n lines of path, or "" if it can't be read.
func tailFile(path string, n int) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
