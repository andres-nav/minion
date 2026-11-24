package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/andres-nav/minion/gru/internal/remote"
	"github.com/andres-nav/minion/gru/internal/sync"
)

func cmdSync(args []string) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	host := hostFlag(fs)
	del := fs.Bool("delete", false, "delete remote files that are not here (except excluded and git-ignored ones)")
	if _, err := parseArgs(fs, args, false); err != nil {
		return 2
	}
	t, err := connect(*host, true)
	if err != nil {
		return errf("%v", err)
	}
	defer t.Close()
	if t.Local {
		fmt.Println("nothing to sync: local host runs in place")
		return 0
	}
	sent, removed, err := pushChanges(t, t.r.Root, t.r.Template.Exclude, t.dir, *del, sync.WalkGit)
	if err != nil {
		return errf("%v", err)
	}
	fmt.Printf("synced %d file(s), removed %d: %s → %s:%s\n", sent, removed, t.r.Root, t.alias, t.dir)
	return 0
}

// pushChanges sends the files of root that walk lists (sync.WalkGit respects
// .gitignore) that differ from remoteDir's, through the agent. With del, remote files absent here are removed, except excluded and
// git-ignored ones. Returns the counts of files sent and removed.
func pushChanges(t *target, root string, exclude []string, remoteDir string, del bool, walk func(string, []string) (sync.Manifest, error)) (int, int, error) {
	// The excludes filter the remote manifest too, so --delete never removes
	// excluded remote files (build output, etc.).
	cmd := t.agent + " _agent manifest " + remote.RemotePath(remoteDir)
	for _, e := range exclude {
		cmd += " " + remote.ShellQuote(e)
	}
	out, err := t.Output(cmd)
	if err != nil {
		return 0, 0, err
	}
	var theirs sync.Manifest
	if err := json.Unmarshal([]byte(out), &theirs); err != nil {
		return 0, 0, fmt.Errorf("bad remote manifest: %w", err)
	}
	ours, err := walk(root, exclude)
	if err != nil {
		return 0, 0, err
	}
	changed := sync.Changed(ours, theirs)
	var deletes []string
	if del {
		deletes = sync.Unignored(root, sync.Deleted(ours, theirs))
	}
	if len(changed) == 0 && len(deletes) == 0 {
		return 0, 0, nil
	}

	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(sync.Send(pw, root, changed, deletes)) }()
	code, err := t.Run(t.agent+" _agent recv "+remote.RemotePath(remoteDir), pr, io.Discard, os.Stderr)
	if err == nil && code != 0 {
		err = fmt.Errorf("remote recv failed (exit %d)", code)
	}
	if err != nil {
		return 0, 0, err
	}
	return len(changed), len(deletes), nil
}
