package config

import (
	"strings"
	"testing"
)

// An argument the program does not understand stops it.
//
// This exists because ignoring one is how the container stack came up with no
// workers. Both worker services ran the server binary with the path of the
// worker as an argument, the server ignored it and started, and the only sign
// was two containers restarting with an error about a setting they do not
// use. The jobs were accepted and nothing ever leased them.
func TestAnArgumentIsRefused(t *testing.T) {
	if err := CheckNoArguments([]string{"/usr/local/bin/quorra-server"}); err != nil {
		t.Fatalf("a program with no arguments was refused: %v", err)
	}
	if err := CheckNoArguments(nil); err != nil {
		t.Fatalf("an empty argument list was refused: %v", err)
	}

	// The exact shape the compose stack produced.
	err := CheckNoArguments([]string{"/usr/local/bin/quorra-server", "/usr/local/bin/quorra-worker"})
	if err == nil {
		t.Fatal("the server accepted the path of another binary as an argument")
	}

	// The message has to name what was given, because the reader is looking
	// at a container that will not stay up and needs to know why.
	if !strings.Contains(err.Error(), "quorra-worker") {
		t.Errorf("the error does not name the argument: %v", err)
	}
	if !strings.Contains(err.Error(), "quorractl") {
		t.Errorf("the error does not say which program does take arguments: %v", err)
	}
}
