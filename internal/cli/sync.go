package cli

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/andres-nav/minion/internal/ssh"
	"github.com/andres-nav/minion/internal/sync"
)

func cmdSync(args []string) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	hostFlag := fs.String("host", "", "override the host")
	del := fs.Bool("delete", false, "delete remote files not present locally (mirror)")
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

	sent, removed, err := pushChanges(rt.client, rt.invoke, r.Root, r.Template.Exclude, rt.dir, *del)
	if err != nil {
		return errf("%v", err)
	}
	if sent == 0 && removed == 0 {
		fmt.Printf("up to date: %s → %s:%s\n", r.Root, rt.alias, rt.dir)
	} else {
		fmt.Printf("synced %d file(s), removed %d: %s → %s:%s\n", sent, removed, r.Root, rt.alias, rt.dir)
	}
	return 0
}

// pushChanges syncs modified files from root to remoteDir over the ssh agent
// (respecting .gitignore). With del, remote files absent locally are removed.
// Returns the counts of files sent and removed.
func pushChanges(client *ssh.Client, invoke, root string, exclude []string, remoteDir string, del bool) (int, int, error) {
	// Pass excludes so the remote manifest is filtered the same way as the local
	// one — otherwise --delete would treat excluded remote files (build output,
	// etc.) as stale and remove them.
	manifestCmd := invoke + " _agent manifest " + remoteDir
	for _, e := range exclude {
		manifestCmd += " " + shellQuote(e)
	}
	remoteRaw, err := client.Output(manifestCmd)
	if err != nil {
		return 0, 0, err
	}
	remote, err := sync.DecodeManifest([]byte(remoteRaw))
	if err != nil {
		return 0, 0, fmt.Errorf("bad remote manifest: %w", err)
	}
	local, err := sync.WalkGit(root, exclude)
	if err != nil {
		return 0, 0, err
	}
	changed := sync.Changed(local, remote)
	var deletes []string
	if del {
		deletes = sync.Deleted(local, remote)
	}
	if len(changed) == 0 && len(deletes) == 0 {
		return 0, 0, nil
	}

	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(sync.Send(pw, root, changed, deletes)) }()

	code, err := client.Run(invoke+" _agent recv "+remoteDir, pr, io.Discard, os.Stderr)
	if err != nil {
		return 0, 0, err
	}
	if code != 0 {
		return 0, 0, fmt.Errorf("remote recv failed (exit %d)", code)
	}
	return len(changed), len(deletes), nil
}
