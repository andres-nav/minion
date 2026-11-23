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

	"github.com/andres-nav/minion/minion/internal/remote"
	"github.com/andres-nav/minion/minion/internal/sync"
)

func cmdFetch(args []string) int {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	host := hostFlag(fs)
	into := fs.String("into", "", "local destination (overrides fetch_into)")
	id, err := parsePositional(fs, args) // optional job id for {id}/{ts}; "" → latest
	if err != nil {
		return 2
	}
	explicit := id != "" // only then is it sure which job's record to fetch

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
			id, _ = rt.exec.Output(remote.LatestJobCommand(rt.state))
		}
		dest = fetchDest(r.Root, r.Template.FetchInto, id, jobStartTS(rt.exec, rt.state, id), filepath.Base(r.Root))
	}

	n, err := pullResults(rt.exec, rt.invoke, remote.RemotePath(rt.dir), r.Template.Fetch, dest)
	if err != nil {
		return errf("%v", err)
	}
	fmt.Printf("fetched %d file(s): %s:%s → %s\n", n, rt.alias, rt.dir, dest)
	if explicit {
		pullJob(rt, remote.JobDir(rt.state, id), dest)
	}
	return 0
}

// pullJob fetches a job's run.json and log into dest/_minion.
func pullJob(rt *remoteTarget, jobDir, dest string) {
	into := filepath.Join(dest, "_minion")
	if _, err := pullResults(rt.exec, rt.invoke, jobDir, []string{"run.json", "log"}, into); err != nil {
		fmt.Fprintf(os.Stderr, "minion: fetch job record failed: %v\n", err)
	}
}

// jobStartTS reads a job's start time and formats it for {ts}, or "" on failure.
func jobStartTS(client execer, state, id string) string {
	if id == "" {
		return ""
	}
	out, err := client.Output("cat " + remote.JobDir(state, id) + "/started")
	if err != nil {
		return ""
	}
	epoch, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return ""
	}
	return stamp(time.Unix(epoch, 0))
}

// pullResults streams files matching patterns from dir (one shell word) back into dest.
func pullResults(client execer, invoke, dir string, patterns []string, dest string) (int, error) {
	cmd := invoke + " _agent send " + dir
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
