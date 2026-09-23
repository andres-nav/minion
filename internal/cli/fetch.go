package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/andres-nav/minion/internal/remote"
	"github.com/andres-nav/minion/internal/sync"
)

func cmdFetch(args []string) int {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	host := hostFlag(fs)
	into := fs.String("into", "", "local destination (overrides fetch_into)")
	id, err := parsePositional(fs, args) // optional job id for {id}/{ts}; "" → latest
	if err != nil {
		return 2
	}

	cfg, r, err := resolveContext()
	if err != nil {
		return errf("%v", err)
	}
	rt, err := dialRemote(cfg, r, *host)
	if err != nil {
		return errf("%v", err)
	}
	defer rt.close()
	if rt.local {
		return errf("fetch is for remote hosts; local runs write results in place")
	}
	if len(r.Template.Fetch) == 0 {
		return errf("template has no fetch patterns")
	}

	dest := expandLocalHome(*into)
	if dest == "" {
		if id == "" { // resolve the latest run for {id}/{ts}
			id, _ = rt.exec.Output(remote.LatestJobCommand())
		}
		dest = fetchDest(r.Root, r.Template.FetchInto, id, jobStartTS(rt.exec, id), filepath.Base(r.Root))
	}

	n, err := pullResults(rt.exec, rt.invoke, rt.dir, r.Template.Fetch, dest)
	if err != nil {
		return errf("%v", err)
	}
	fmt.Printf("fetched %d file(s): %s:%s → %s\n", n, rt.alias, rt.dir, dest)
	return 0
}

// jobStartTS reads a job's start time and formats it for {ts}, or "" on failure.
func jobStartTS(client execer, id string) string {
	if id == "" {
		return ""
	}
	out, err := client.Output("cat " + remote.JobDir(id) + "/started")
	if err != nil {
		return ""
	}
	epoch, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return ""
	}
	return time.Unix(epoch, 0).Local().Format(tsLayout)
}

// pullResults streams files matching patterns from remoteDir back into dest.
func pullResults(client execer, invoke, remoteDir string, patterns []string, dest string) (int, error) {
	cmd := invoke + " _agent send " + remote.RemotePath(remoteDir)
	for _, p := range patterns {
		cmd += " " + remote.ShellQuote(p)
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
