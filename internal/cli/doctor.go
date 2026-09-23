package cli

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/andres-nav/minion/internal/config"
	"github.com/andres-nav/minion/internal/remote"
	"github.com/andres-nav/minion/internal/ssh"
)

// cmdDoctor validates the config and checks each host: reachable + agent
// bootstraps. It replaces the old ad-hoc reachability check.
func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	host := hostFlag(fs)
	notify := fs.Bool("notify", false, "also fire a test notification (email and/or run hooks) from --host or the template's default host")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, r, err := resolveContext()
	if err != nil {
		return errf("%v", err)
	}

	problems := 0

	// config sanity
	if err := cfg.Notify.Validate(); err != nil {
		fmt.Printf("notify   %-14s FAIL  %v\n", "(default)", err)
		problems++
	}
	for name, t := range cfg.Templates {
		if len(t.OrderedTasks()) == 0 {
			fmt.Printf("template %-14s WARN  no tasks defined\n", name)
		}
		if t.Host != "" {
			if _, ok := cfg.Hosts[t.Host]; !ok {
				fmt.Printf("template %-14s note  host %q not in hosts (used as a raw ssh alias)\n", name, t.Host)
			}
		}
		if err := t.Notify.Validate(); err != nil {
			fmt.Printf("template %-14s FAIL  %v\n", name, err)
			problems++
		}
	}

	// which hosts to check
	var names []string
	if *host != "" {
		names = []string{*host}
	} else {
		for n := range cfg.Hosts {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		fmt.Println("no hosts configured")
	}

	for _, n := range names {
		alias := n
		if h, ok := cfg.Hosts[n]; ok && h.SSH != "" {
			alias = h.SSH
		}
		if isLocalAlias(alias) {
			fmt.Printf("host %-14s OK    local (this machine)\n", n)
			continue
		}
		c := ssh.New(alias)
		unm, err := c.Output("uname -sm")
		if err != nil {
			fmt.Printf("host %-14s FAIL  unreachable\n", n)
			problems++
			continue
		}
		invoke, err := c.EnsureAgent(version, agentBinaryFor)
		if err != nil {
			fmt.Printf("host %-14s FAIL  agent: %v\n", n, err)
			problems++
			continue
		}
		fmt.Printf("host %-14s OK    %s, agent %s\n", n, unm, invoke)
	}

	// optional end-to-end notification check (toggle: --notify)
	if *notify {
		if code := sendNotifyTest(cfg, r, *host); code != 0 {
			problems++
		}
	}

	if problems > 0 {
		fmt.Fprintf(os.Stderr, "\n%d problem(s)\n", problems)
		return 1
	}
	return 0
}

// sendNotifyTest fires a synthetic notification through the resolved config
// (email and/or run hooks) from the target host, verifying credentials and
// connectivity end to end. Invoked by `doctor --notify`. Returns a process exit
// code (0 on success).
func sendNotifyTest(cfg *config.Config, r *config.Resolved, host string) int {
	n := cfg.NotifyFor(r.Template)
	if n == nil {
		return errf("no notify configured (add a notify.email or notify.run block to the template or global config)")
	}

	rt, err := dialRemote(cfg, r, host)
	if err != nil {
		return errf("%v", err)
	}
	defer rt.close()

	invoke := notifyInvoke(rt)
	var b strings.Builder
	// synthetic job env so the message reads like a real (successful) run.
	b.WriteString("export MINION_JOB_ID=notify-test MINION_TASK=notify-test MINION_HOST=")
	b.WriteString(remote.ShellQuote(rt.alias))
	b.WriteString(" MINION_EXIT=0 MINION_DURATION=0\n")
	// Force the gate open (a synthetic exit=0 would otherwise skip an on:failure
	// notify) so the test always exercises the configured transports.
	test := *n
	test.On = "always"
	b.WriteString(buildNotify(invoke, "", &test))

	code, err := rt.exec.Run(b.String(), nil, os.Stdout, os.Stderr)
	if err != nil {
		return errf("notify-test: %v", err)
	}
	if code == 0 {
		fmt.Fprintf(os.Stderr, "fired test notification via %s\n", rt.alias)
	}
	return code
}
