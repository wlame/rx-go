// Command gen writes and checks the invented journal fixtures of
// internal/journal. Every value it writes is made up here; it never reads
// a real journal.
//
// It runs inside the image built from the Dockerfile beside it, where
// make.sh calls it between systemd-journal-remote and journalctl. Start
// the whole run with `just journal-fixtures` from the repository root.
//
//	gen streams --out=DIR
//	    write the export stream of every set (DIR/basic.export, …)
//	gen trim --in=FILE --out=FILE
//	    cut a journal to its used size and record that size in its header
//	gen check --sent=FILE --stored=FILE --journal=FILE --compression=NAME --compact=0|1
//	    check that every field sent reached the journal, in the variant asked for
//	gen manifest --work=DIR
//	    print fixtures.json for the journals under DIR
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// command is one subcommand: its name, a one-line description for the
// usage text, and the function that parses its flags and runs it.
type command struct {
	name    string
	summary string
	run     func(args []string) error
}

// commands is the table main dispatches on.
var commands = []command{
	{"streams", "--out=DIR: write the export stream of every set", runStreams},
	{"trim", "--in=FILE --out=FILE: cut a journal to its used size", runTrim},
	{"check", "--sent=FILE --stored=FILE --journal=FILE --compression=NAME --compact=0|1: check a written journal", runCheck},
	{"manifest", "--work=DIR: print fixtures.json for the journals under DIR", runManifest},
}

// errUsage marks an error in the command line rather than in the work.
var errUsage = errors.New("usage")

func main() {
	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		if errors.Is(err, errUsage) {
			fmt.Fprint(os.Stderr, usage())
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// dispatch runs the subcommand args[0] names with the rest of args.
func dispatch(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: no command", errUsage)
	}
	for _, c := range commands {
		if c.name == args[0] {
			return c.run(args[1:])
		}
	}
	return fmt.Errorf("%w: unknown command %q", errUsage, args[0])
}

// usage returns the command table as the usage text.
func usage() string {
	var b strings.Builder
	b.WriteString("usage: gen COMMAND [FLAGS]\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "  gen %s %s\n", c.name, c.summary)
	}
	return b.String()
}

// parseFlags parses args into fs and fails when one of the required
// string flags is empty. fs's flags are bound to variables by the caller
// (fs.String returns a pointer the parse writes through).
func parseFlags(fs *flag.FlagSet, args []string, required ...string) error {
	// ContinueOnError makes Parse return the error instead of exiting,
	// so main prints one message and the usage.
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %s: %v", errUsage, fs.Name(), err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: %s: unexpected arguments %q", errUsage, fs.Name(), fs.Args())
	}
	var missing []string
	for _, name := range required {
		if fs.Lookup(name).Value.String() == "" {
			missing = append(missing, "--"+name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s needs %s", errUsage, fs.Name(), strings.Join(missing, " "))
	}
	return nil
}
