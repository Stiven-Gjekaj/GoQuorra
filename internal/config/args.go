package config

import (
	"fmt"
	"strings"
)

// CheckNoArguments refuses an argument that a binary does not understand.
//
// Neither the server nor the worker takes any. Ignoring what it is given
// sounds harmless and is not, because it turns a mistake that should stop the
// process into one that starts the wrong program.
//
// That is not hypothetical. The image holds three binaries, and the two
// orchestrators spell the choice of one with the same word: `command:` in
// Kubernetes replaces the entrypoint, and `command:` in Docker Compose
// replaces CMD. With an entrypoint set, a compose service asking for the
// worker ran the server with the path of the worker as an argument. The
// server ignored the argument and started, so the stack came up with no
// workers at all, and the only sign was two containers restarting with an
// error about a setting they do not use.
//
// args is os.Args, so the first element is the name of the program.
func CheckNoArguments(args []string) error {
	if len(args) <= 1 {
		return nil
	}

	name := "this program"
	if len(args) > 0 && args[0] != "" {
		name = args[0]
	}

	return fmt.Errorf(
		"config: %s takes no arguments and was given %s. It is configured through the environment, and quorractl is the program that takes arguments",
		name, strings.Join(args[1:], " "))
}
