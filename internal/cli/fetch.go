package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/andres-nav/minion/internal/sync"
)

func cmdFetch(args []string) int {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	hostFlag := fs.String("host", "", "override the host")
	into := fs.String("into", "", "local destination directory (default: project root)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, r, err := resolveContext()
	if err != nil {
		return errf("%v", err)
	}
	rt, err := dialRemote(cfg, r, *hostFlag)
	if err != nil {
		return errf("%v", err)
	}
	if rt.local {
		return errf("fetch is for remote hosts; local runs write results in place")
	}
	if len(r.Template.Fetch) == 0 {
		return errf("template has no fetch patterns")
	}

	dest := r.Root
	if *into != "" {
		dest = *into
	}

	n, err := pullResults(rt.exec, rt.invoke, rt.dir, r.Template.Fetch, dest)
	if err != nil {
		return errf("%v", err)
	}
	fmt.Printf("fetched %d file(s): %s:%s → %s\n", n, rt.alias, rt.dir, dest)
	return 0
}

// pullResults streams files matching patterns from remoteDir back into dest.
func pullResults(client execer, invoke, remoteDir string, patterns []string, dest string) (int, error) {
	cmd := invoke + " _agent send " + remoteDir
	for _, p := range patterns {
		cmd += " " + shellQuote(p)
	}

	count := 0
	code, err := client.RunStdout(cmd, os.Stderr, func(rd io.Reader) error {
		n, e := sync.Receive(rd, dest)
		count = n
		return e
	})
	if err != nil {
		return 0, err
	}
	if code != 0 {
		return 0, fmt.Errorf("remote send failed (exit %d)", code)
	}
	return count, nil
}

// shellQuote single-quotes s so the remote shell passes it literally (patterns
// must not be glob-expanded by the shell before the agent sees them).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
