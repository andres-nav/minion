package cli

import (
	"cmp"
	"flag"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/andres-nav/minion/gru/internal/config"
	"github.com/andres-nav/minion/gru/internal/remote"
)

// cmdDoctor checks the config and each host: reachable, and its agent and
// minion in place.
func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	host := hostFlag(fs)
	notify := fs.Bool("notify", false, "also run the notify hooks as a test from --host or the template's host")
	if _, err := parseArgs(fs, args, false); err != nil {
		return 2
	}
	r, err := project() // loads and checks the config
	if err != nil {
		return errf("%v", err)
	}
	names := slices.Sorted(maps.Keys(r.Config.Hosts))
	if *host != "" {
		names = []string{*host}
	}
	if len(names) == 0 {
		fmt.Println("no hosts configured")
	}
	problems := 0
	for _, n := range names {
		t, err := dial(r, n, true)
		if err != nil {
			fmt.Printf("host %-14s FAIL  %v\n", n, err)
			problems++
			continue
		}
		agent, minion := t.agent, t.minion
		if p, err := t.Output("printf '%s\\n' " + t.agent + " " + t.minion); err == nil {
			agent, minion, _ = strings.Cut(p, "\n")
		}
		root := "no minion for this platform"
		if t.minion != "" {
			root = minion + ", root: " + cmp.Or(serveProbe(t), "set up")
		}
		t.Close()
		fmt.Printf("host %-14s OK    %s, agent %s, minion %s\n", n, t.alias, agent, root)
	}
	if *notify && sendNotifyTest(r, *host) != 0 {
		problems++
	}
	if problems > 0 {
		fmt.Fprintf(os.Stderr, "\n%d problem(s)\n", problems)
		return 1
	}
	return 0
}

// sendNotifyTest runs the resolved block's hooks as a test notification
// (whatever its `on`) from the target host, to check them end to end.
func sendNotifyTest(r *config.Resolved, host string) int {
	n := r.Config.NotifyFor(r.Template)
	if n == nil {
		return errf("no notify configured (add a notify block with run hooks to the template or the config)")
	}
	test := *n
	test.On = "always"
	block, err := resolvedNotify(&test)
	if err != nil {
		return errf("notify: %v", err)
	}
	t, err := dial(r, host, true)
	if err != nil {
		return errf("%v", err)
	}
	defer t.Close()
	cmd := "export GRU_JOB_ID=notify-test GRU_TASK=notify-test GRU_HOST=" + remote.ShellQuote(t.alias) +
		" GRU_EXIT=0 GRU_DURATION=0; " + t.agent + " _agent notify --config -"
	code, err := t.Run(cmd, strings.NewReader(block), os.Stdout, os.Stderr)
	if err != nil {
		return errf("notify test: %v", err)
	}
	if code == 0 {
		fmt.Fprintf(os.Stderr, "sent a test notification from %s\n", t.alias)
	}
	return code
}
