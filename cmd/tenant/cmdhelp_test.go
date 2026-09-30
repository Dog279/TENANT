package main

import (
	"errors"
	"flag"
	"fmt"
	"testing"
)

// `tenant <command> --help` shows the command's help page when the command
// read the flag as a subcommand or a task and failed; a command's own flag set
// has already answered it (flag.ErrHelp), and other errors stay errors.
func TestHelpInsteadOfAUsageError(t *testing.T) {
	usage := errors.New("unknown subcommand \"--help\"")
	for _, tc := range []struct {
		err  error
		args []string
		want bool
	}{
		{usage, []string{"--help"}, true},
		{usage, []string{"-h"}, true},
		{usage, []string{"-help", "x"}, true},
		{fmt.Errorf("wrapped: %w", flag.ErrHelp), []string{"--help"}, false},
		{usage, []string{"list", "--help"}, false},
		{usage, nil, false},
		{nil, []string{"--help"}, false},
	} {
		if got := helpInstead(tc.err, tc.args); got != tc.want {
			t.Errorf("helpInstead(%v, %q) = %v, want %v", tc.err, tc.args, got, tc.want)
		}
	}
}
