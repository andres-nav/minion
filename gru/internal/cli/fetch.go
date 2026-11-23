package cli

import (
	"cmp"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/andres-nav/minion/gru/internal/remote"
	"github.com/andres-nav/minion/gru/internal/sync"
)

// cmdFetch copies the files matching `fetch` from the project's remote dir, as
// they are now. A job id (default: the latest of this project) names the
// destination's {id}/{ts} and adds that job's run.json and log under _gru/.
func cmdFetch(args []string) int {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	host := hostFlag(fs)
	into := fs.String("into", "", "local destination (overrides fetch_into)")
	id, err := parsePositional(fs, args)
	if err != nil {
		return 2
	}
	r, err := project()
	if err != nil {
		return errf("%v", err)
	}
	if len(r.Template.Fetch) == 0 {
		return errf("the template has no fetch patterns")
	}
	t, err := dial(r, *host, true)
	if err != nil {
		return errf("%v", err)
	}
	defer t.Close()
	if t.Local {
		return errf("fetch is for remote hosts; local runs write results in place")
	}
	if id, err = jobID(t, id); err != nil {
		return errf("%v", err)
	}
	dest := cmp.Or(expandLocalHome(*into), fetchDest(r.Root, r.Template.FetchInto, id))
	return exit(0, fetchJob(t, id, dest))
}

// fetchJob pulls the fetch files from the project dir into dest and, given
// an id, that job's run.json and log into dest/_gru.
func fetchJob(t *target, id, dest string) error {
	n, err := pullResults(t, remote.RemotePath(t.dir), t.r.Template.Fetch, dest)
	if err == nil {
		fmt.Fprintf(os.Stderr, "fetched %d file(s): %s:%s → %s\n", n, t.alias, t.dir, dest)
	}
	if id != "" {
		if _, jerr := pullResults(t, remote.JobDir(t.state, id), []string{"run.json", "log"}, filepath.Join(dest, "_gru")); jerr != nil {
			fmt.Fprintf(os.Stderr, "gru: fetch job record failed: %v\n", jerr)
		}
	}
	return err
}

// pullResults streams files matching patterns from dir (one shell word) back into dest.
func pullResults(t *target, dir string, patterns []string, dest string) (int, error) {
	cmd := t.agent + " _agent send " + dir
	for _, p := range patterns {
		cmd += " " + remote.ShellQuote(p)
	}
	count := 0
	code, err := t.RunStdout(cmd, os.Stderr, func(rd io.Reader) (e error) {
		count, e = sync.Receive(rd, dest)
		return e
	})
	if err == nil && code != 0 {
		err = fmt.Errorf("remote send failed (exit %d)", code)
	}
	return count, err
}
