package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/andres-nav/minion/minion/internal/config"
	"github.com/andres-nav/minion/minion/internal/notify"
	"github.com/andres-nav/minion/minion/internal/sync"
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
	case "now":
		fmt.Println(time.Now().UnixNano())
		return 0
	default:
		fmt.Fprintf(os.Stderr, "minion _agent: unknown subcommand %q\n", args[0])
		return 2
	}
}

// agentNotify announces a finished job as the notify block in --config says (a
// resolved block as JSON, in a file or - for stdin). The outcome comes from the
// MINION_* env the run wrapper exported. Run hooks also see $MINION_SUBJECT,
// $MINION_BODY and the block's env.
func agentNotify(args []string) int {
	fs := flag.NewFlagSet("notify", flag.ContinueOnError)
	path := fs.String("config", "", "notify block as JSON: a file, or - for stdin")
	logPath := fs.String("log", "", "job log file, tailed into the message body")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *path == "" {
		fmt.Fprintln(os.Stderr, "minion _agent notify: missing --config")
		return 2
	}
	n, err := readNotify(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "minion _agent notify: %v\n", err)
		return 1
	}
	res := notify.Result{
		JobID:    os.Getenv("MINION_JOB_ID"),
		Task:     os.Getenv("MINION_TASK"),
		Host:     os.Getenv("MINION_HOST"),
		Exit:     atoiOr(os.Getenv("MINION_EXIT"), -1),
		Duration: atoiOr(os.Getenv("MINION_DURATION"), 0),
		LogTail:  tailFile(*logPath, 30),
	}
	if !n.Fires(res.Exit) {
		return 0
	}
	code := 0
	if e := n.Email; e != nil {
		sink := notify.Email{Addr: e.SMTP, From: e.From, To: splitList(e.To), User: e.User, Pass: e.Pass}
		if err := sink.Send(res.Subject(), res.Body()); err != nil {
			fmt.Fprintf(os.Stderr, "[minion] notify: email failed: %v\n", err)
			code = 1
		}
	}
	for _, c := range n.Run {
		cmd := exec.Command("sh", "-c", c)
		cmd.Env = append(os.Environ(), "MINION_SUBJECT="+res.Subject(), "MINION_BODY="+res.Body())
		for _, k := range sortedKeys(n.Env) {
			cmd.Env = append(cmd.Env, k+"="+n.Env[k])
		}
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "[minion] notify: run hook failed: %v\n", err)
			code = 1
		}
	}
	return code
}

func readNotify(path string) (*config.Notify, error) {
	r := io.Reader(os.Stdin)
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	n := &config.Notify{}
	if err := json.NewDecoder(r).Decode(n); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return n, nil
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
