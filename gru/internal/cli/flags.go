package cli

import "flag"

// hostFlag registers the standard --host flag shared by the remote commands, so
// every command describes it identically.
func hostFlag(fs *flag.FlagSet) *string {
	return fs.String("host", "", "target host: a hosts-map name, an ssh alias, or localhost")
}

// parsePositional parses fs and returns the command's single optional
// positional argument (an id or task name), which may be surrounded by flags on
// either side. flag.Parse stops at the first non-flag token, so a second pass
// over whatever trails the positional is what makes `kill <id> --host x` work.
func parsePositional(fs *flag.FlagSet, args []string) (string, error) {
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	pos := fs.Arg(0)
	if fs.NArg() > 1 {
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return "", err
		}
	}
	return pos, nil
}
