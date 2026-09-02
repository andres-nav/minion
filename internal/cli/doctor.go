package cli

import (
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/andres-nav/minion/internal/ssh"
)

// cmdDoctor validates the config and checks each host: reachable + agent
// bootstraps. It replaces the old ad-hoc reachability check.
func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	host := fs.String("host", "", "check only this host")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, _, err := resolveContext()
	if err != nil {
		return errf("%v", err)
	}

	problems := 0

	// config sanity
	for name, t := range cfg.Templates {
		if len(t.OrderedTasks()) == 0 {
			fmt.Printf("template %-14s WARN  no tasks defined\n", name)
		}
		if t.Host != "" {
			if _, ok := cfg.Hosts[t.Host]; !ok {
				fmt.Printf("template %-14s note  host %q not in hosts (used as a raw ssh alias)\n", name, t.Host)
			}
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

	if problems > 0 {
		fmt.Fprintf(os.Stderr, "\n%d problem(s)\n", problems)
		return 1
	}
	return 0
}
