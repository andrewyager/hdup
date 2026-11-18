package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
)

func TestExitGivesTheCodeAndTheMessageForEachResult(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		ended bool // the root context has ended
		code  int
		msg   string
	}{
		{"done", nil, false, 0, ""},
		{"interrupted", fmt.Errorf("run: %w", context.Canceled), false, 130, "interrupted; run the same command again to continue"},
		{"network error after interrupt", &net.OpError{Op: "dial", Err: errors.New("use of closed connection")}, true, 130, "interrupted; run the same command again to continue"},
		{"done after interrupt", nil, true, 0, ""},
		{"failed", errors.New("2 files failed"), false, 1, "2 files failed"},
		{"error text with a control character", errors.New("list \x1b[2Jx: failed"), false, 1, `"list \x1b[2Jx: failed"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if c.ended {
				cancel()
			}
			if code, msg := exit(ctx, c.err); code != c.code || msg != c.msg {
				t.Fatalf("got %d, %q", code, msg)
			}
		})
	}
}
