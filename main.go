// Command minion is a thin remote dev-loop orchestrator. See DESIGN.md.
package main

import (
	"os"

	"github.com/andres-nav/minion/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
