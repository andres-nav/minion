// Command gru is a thin remote dev-loop orchestrator. See docs/GRU.md.
package main

import (
	"os"

	"github.com/andres-nav/minion/gru/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
