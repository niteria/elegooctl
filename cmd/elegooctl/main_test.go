package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestHelpDoesNotNeedPrinter(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"--help"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut.String(), "snapshot") {
		t.Fatal("missing command help")
	}
}

func TestInvalidInputs(t *testing.T) {
	for _, args := range [][]string{{"status"}, {"--host", "example.invalid", "query", "--command", "128"}, {"--host", "example.invalid", "snapshot"}, {"--host", "example.invalid", "status", "unexpected"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out bytes.Buffer
			if err := run(context.Background(), args, &out, &out); err == nil {
				t.Fatal("accepted invalid command")
			}
		})
	}
}
