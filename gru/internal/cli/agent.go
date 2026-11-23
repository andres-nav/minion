package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/andres-nav/minion/gru/internal/config"
	"github.com/andres-nav/minion/gru/internal/notify"
	"github.com/andres-nav/minion/gru/internal/sync"
)

// cmdAgent is the hidden counterpart that gru runs on the target.
func cmdAgent(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "gru _agent: missing subcommand")
		return 2
	}
	verb, args := args[0], args[1:]
	switch verb {
	case "version":
		fmt.Printf("gru %s\n", version)
		return 0
	case "now":
		fmt.Println(time.Now().UnixNano())
		return 0
	case "notify":
		return agentNotify(args)
	case "follow", "manifest", "recv", "send":
	default:
		fmt.Fprintf(os.Stderr, "gru _agent: unknown subcommand %q\n", verb)
		return 2
	}
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "gru _agent %s: missing dir\n", verb)
		return 2
	}
	dir := args[0]
	var err error
	switch verb {
	case "follow":
		var code int
		if code, err = follow(dir, os.Stdout); err == nil {
			return code
		}
	case "manifest":
		var m sync.Manifest
		if m, err = sync.Walk(dir, args[1:]); err == nil {
			err = json.NewEncoder(os.Stdout).Encode(m)
		}
	case "recv":
		_, err = sync.Receive(os.Stdin, dir)
	case "send":
		var paths []string
		if paths, err = sync.Match(dir, args[1:]); err == nil {
			err = sync.Send(os.Stdout, dir, paths, nil)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "gru _agent %s: %v\n", verb, err)
		return 1
	}
	return 0
}

// notifyTimeout bounds a job-end notification: email and run hooks together.
var notifyTimeout = 30 * time.Second

// agentNotify announces a finished job as the notify block in --config says (a
// resolved block as JSON, in a file or - for stdin). The outcome comes from the
// GRU_* env the job wrapper exported, unless --subject/--body give the
// message: then it is sent as is, without the `on` gate (for a task's own
// events). Run hooks also see $GRU_SUBJECT, $GRU_BODY and the block's env.
// Whatever still runs after notifyTimeout is killed.
func agentNotify(args []string) int {
	fs := flag.NewFlagSet("notify", flag.ContinueOnError)
	path := fs.String("config", "", "notify block as JSON: a file, or - for stdin")
	logPath := fs.String("log", "", "job log file, tailed into the message body")
	subject := fs.String("subject", "", "the message's subject, sent without the on gate")
	body := fs.String("body", "", "the message's body, sent without the on gate")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *path == "" {
		fmt.Fprintln(os.Stderr, "gru _agent notify: missing --config")
		return 2
	}
	n, err := readNotify(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gru _agent notify: %v\n", err)
		return 1
	}
	res := notify.Result{
		JobID:    os.Getenv("GRU_JOB_ID"),
		Task:     os.Getenv("GRU_TASK"),
		Host:     os.Getenv("GRU_HOST"),
		Exit:     atoiOr(os.Getenv("GRU_EXIT"), -1),
		Duration: atoiOr(os.Getenv("GRU_DURATION"), 0),
		LogTail:  tailFile(*logPath, 30),
	}
	subj, text := res.Subject(), res.Body()
	if *subject != "" || *body != "" {
		subj, text = *subject, *body
	} else if !n.Fires(res.Exit) {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()
	code := 0
	if e := n.Email; e != nil {
		sent := make(chan error, 1)
		go func() {
			sent <- notify.Email{Addr: e.SMTP, From: e.From, To: splitList(e.To), User: e.User, Pass: e.Pass}.Send(subj, text)
		}()
		select {
		case err = <-sent:
		case <-ctx.Done():
			err = ctx.Err()
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "[gru] notify: email failed: %v\n", err)
			code = 1
		}
	}
	for _, c := range n.Run {
		cmd := exec.CommandContext(ctx, "sh", "-c", c)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.Env = append(os.Environ(), "GRU_SUBJECT="+subj, "GRU_BODY="+text)
		for k, v := range n.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "[gru] notify: run hook failed: %v\n", err)
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
