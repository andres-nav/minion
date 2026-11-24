package cli

import (
	"errors"
	"flag"
)

// hostFlag registers the --host flag shared by the remote commands.
func hostFlag(fs *flag.FlagSet) *string {
	return fs.String("host", "", "target host: a hosts-map name, an ssh alias, or localhost")
}

// parseArgs parses fs, with flags on either side of the command's one optional
// positional argument (an id or task name) if it takes one. Any other argument
// is a usage error, so no flag after it is silently dropped.
func parseArgs(fs *flag.FlagSet, args []string, positional bool) (string, error) {
	err := fs.Parse(args)
	pos := fs.Arg(0)
	if err == nil && positional && fs.NArg() > 0 {
		err = fs.Parse(fs.Args()[1:])
	}
	if err == nil && fs.NArg() > 0 {
		fs.Usage()
		err = errors.New("too many arguments")
	}
	return pos, err
}
