package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCommand(t *testing.T) {
	for _, tc := range []struct {
		args        []string
		code        int
		done        bool
		out, errOut string
	}{
		{args: nil, code: 0, done: false},
		{args: []string{"version"}, code: 0, done: true, out: "audiosilo dev\n"},
		{args: []string{"foo"}, code: 2, done: true, errOut: `unknown command "foo"`},
		{args: []string{"version", "extra"}, code: 2, done: true, errOut: `unknown command "version"`},
	} {
		var out, errOut bytes.Buffer
		code, done := command(tc.args, &out, &errOut)
		if code != tc.code || done != tc.done {
			t.Errorf("command(%q) = %d, %v; want %d, %v", tc.args, code, done, tc.code, tc.done)
		}
		if out.String() != tc.out {
			t.Errorf("command(%q) stdout = %q, want %q", tc.args, out.String(), tc.out)
		}
		if !strings.Contains(errOut.String(), tc.errOut) || (tc.errOut == "" && errOut.Len() > 0) {
			t.Errorf("command(%q) stderr = %q, want %q", tc.args, errOut.String(), tc.errOut)
		}
	}
}
