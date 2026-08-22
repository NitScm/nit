package main

import (
	"flag"
	"strings"
)

// parseFlags parses args in any order and returns the positional arguments.
//
// The standard flag package stops parsing at the first non-flag argument, so
// "nit clone backend-api -branch main" would silently ignore -branch. People do
// not write commands that way — git, gh and docker all accept flags after
// positionals — and an option that is quietly dropped is worse than one that
// errors.
//
// Flags are therefore moved ahead of positionals before parsing. Everything
// after "--" is positional, whatever it looks like, so a path beginning with a
// dash stays a path.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var flags, positional []string

	for i := 0; i < len(args); i++ {
		arg := args[i]

		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}

		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}

		flags = append(flags, arg)

		// "-flag value" carries its value in the next argument, unless the
		// value is attached ("-flag=value") or the flag is a boolean.
		if !strings.Contains(arg, "=") && takesValue(fs, arg) && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}

	if err := fs.Parse(append(flags, positional...)); err != nil {
		return nil, err
	}

	return fs.Args(), nil
}

// takesValue reports whether a flag expects a separate value argument.
func takesValue(fs *flag.FlagSet, arg string) bool {
	name := strings.TrimLeft(arg, "-")

	found := fs.Lookup(name)
	if found == nil {
		// Unknown flags are left for flag.Parse to reject with its own message,
		// which names the flag and prints the usage.
		return false
	}

	boolFlag, ok := found.Value.(interface{ IsBoolFlag() bool })

	return !ok || !boolFlag.IsBoolFlag()
}
