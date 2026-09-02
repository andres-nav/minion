package cli

import (
	"fmt"
	"os"

	"github.com/andres-nav/minion/internal/sync"
)

// cmdAgent is the hidden remote-side counterpart invoked over ssh.
func cmdAgent(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "minion _agent: missing subcommand")
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Printf("minion %s\n", version)
		return 0
	case "manifest":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "minion _agent manifest: missing dir")
			return 2
		}
		if err := sync.WriteManifest(os.Stdout, args[1], args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "minion _agent: %v\n", err)
			return 1
		}
		return 0
	case "recv":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "minion _agent recv: missing dir")
			return 2
		}
		n, err := sync.Receive(os.Stdin, args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "minion _agent: %v\n", err)
			return 1
		}
		fmt.Printf("%d\n", n)
		return 0
	case "send":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "minion _agent send: missing dir")
			return 2
		}
		paths, err := sync.Match(args[1], args[2:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "minion _agent: %v\n", err)
			return 1
		}
		if err := sync.Send(os.Stdout, args[1], paths, nil); err != nil {
			fmt.Fprintf(os.Stderr, "minion _agent: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "minion _agent: unknown subcommand %q\n", args[0])
		return 2
	}
}
