// Package exitcode carries a process exit status through an error value.
//
// The command bodies under cli/internal used to be main packages that
// returned an int for os.Exit. They now return an error so a host program
// can mount them as subcommands, but their exit-code contracts (2 for usage
// errors, 1 for failures, orpheus-say's 130 on interrupt, the --validate
// scripting contract) are what deploy tooling branches on, so the status
// travels inside the error rather than being flattened to 1.
package exitcode

import (
	"errors"
	"flag"
	"fmt"
)

// Error is a request to exit with Code. The body that returns it has already
// printed its diagnostic (to stderr or its logger) exactly as the standalone
// binary always did, so a wrapper should exit with Code without printing
// Error() again.
type Error struct{ Code int }

func (e *Error) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// Status converts a legacy exit code into an error: nil for 0, otherwise an
// *Error carrying the code.
func Status(code int) error {
	if code == 0 {
		return nil
	}
	return &Error{Code: code}
}

// Parse maps a flag.FlagSet.Parse failure. flag.ErrHelp is passed through so
// callers can tell "help requested" from a bad flag; anything else has already
// been reported by the flag package (message plus usage on its output) and is
// the usage exit status 2, as the binaries have always returned.
func Parse(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return err
	}
	return Status(2)
}

// Code is the exit status a standalone binary reports for err: 0 for nil,
// the carried code for an *Error, 2 for flag.ErrHelp (flag.ContinueOnError
// plus "return 2" is what every binary has always done on --help), and 1 for
// any other error.
func Code(err error) int {
	if err == nil {
		return 0
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if errors.Is(err, flag.ErrHelp) {
		return 2
	}
	return 1
}
