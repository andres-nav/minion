package cli

import (
	"flag"
	"fmt"
	"io"
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
	case "summary":
		return agentSummary(args[1:])
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

// agentSummary prints the job summary composed from the MINION_* env — the same
// text the email transport uses — so run hooks (e.g. a curl webhook) can send a
// consistent message via $MINION_SUBJECT / $MINION_BODY. Default prints the
// one-line subject; --body prints the full body (with --log tailed into it).
func agentSummary(args []string) int {
	fs := flag.NewFlagSet("summary", flag.ContinueOnError)
	body := fs.Bool("body", false, "print the full body instead of the one-line subject")
	logPath := fs.String("log", "", "job log file, tailed into the body")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	res := notify.Result{
		JobID:    os.Getenv("MINION_JOB_ID"),
		Task:     os.Getenv("MINION_TASK"),
		Host:     os.Getenv("MINION_HOST"),
		Exit:     atoiOr(os.Getenv("MINION_EXIT"), -1),
		Duration: atoiOr(os.Getenv("MINION_DURATION"), 0),
	}
	if *body {
		res.LogTail = tailFile(*logPath, 30)
		fmt.Print(res.Body())
	} else {
		fmt.Println(res.Subject())
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

// tailFile returns roughly the last n lines of path, or "" if it can't be read.
// It reads only the tail of the file (bounded by maxTail), so a job that emits a
// huge log doesn't get slurped entirely into memory just to quote a few lines.
func tailFile(path string, n int) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	const maxTail = 64 << 10 // enough for n normal-length lines
	start := int64(0)
	if fi.Size() > maxTail {
		start = fi.Size() - maxTail
	}
	buf := make([]byte, fi.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:] // drop a possibly-partial first line too
	}
	return strings.Join(lines, "\n")
}
