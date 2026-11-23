package cli

import (
	"errors"
	"flag"
)

// hostFlag registers the --host flag shared by the remote commands.
func hostFlag(fs *flag.FlagSet) *string {
	return fs.String("host", "", "target host: a hosts-map name, an ssh alias, or localhost")
}

// parsePositional parses fs and returns the command's one optional positional
// argument (an id or task name), with flags on either side of it.
func parsePositional(fs *flag.FlagSet, args []string) (string, error) {
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	pos := fs.Arg(0)
	if fs.NArg() > 1 {
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return "", err
		}
		if fs.NArg() > 0 {
			fs.Usage()
			return "", errors.New("too many arguments")
		}
	}
	return pos, nil
}
