package asuser

import (
	"strings"
	"syscall"
	"testing"
)

func TestResultRoundTrip(t *testing.T) {
	tests := []struct {
		r    Result
		exit int
	}{
		{Result{StepOK, 0}, 0},
		{Result{StepMkdirCreated, 0}, 0},
		{Result{StepRelabel, syscall.EACCES}, 0},
		{Result{StepMkdir, syscall.ENOENT}, 1},
		{Result{StepTempfile, syscall.ENOSPC}, 1},
		{Result{StepOpen, syscall.EACCES}, 1},
		{Result{StepCopy, syscall.EDQUOT}, 1},
		{Result{StepClose, syscall.EIO}, 1},
		{Result{StepChmod, syscall.EPERM}, 1},
		{Result{StepChmod, 4095}, 1},
	}
	for _, tc := range tests {
		line := tc.r.String() + "\n"
		got, ok := ParseResult([]byte(line), tc.exit)
		if !ok || got != tc.r {
			t.Errorf("ParseResult(%q, %d) = %+v, %v; want %+v", line, tc.exit, got, ok, tc.r)
		}
	}
	if s := (Result{StepOpen, syscall.EACCES}).String(); s != "open 13" {
		t.Errorf("String = %q; want \"open 13\"", s)
	}
}

func TestParseResultRefuses(t *testing.T) {
	tests := []struct {
		name string
		out  string
		exit int
	}{
		{"empty", "", 0},
		{"no newline", "ok 0", 0},
		{"two lines", "ok 0\nok 0\n", 0},
		{"blank second line", "ok 0\n\n", 0},
		{"over 64 bytes", "ok 0" + strings.Repeat(" ", 60) + "\n", 0},
		{"unknown step", "done 0\n", 0},
		{"step case", "OK 0\n", 0},
		{"errno 0 on a failure step", "open 0\n", 1},
		{"errno 0 on relabel", "relabel 0\n", 0},
		{"errno on ok", "ok 13\n", 0},
		{"errno on mkdir-created", "mkdir-created 1\n", 0},
		{"errno above 4095", "open 4096\n", 1},
		{"negative errno", "open -13\n", 1},
		{"plus sign", "open +13\n", 1},
		{"leading zero", "open 013\n", 1},
		{"hex errno", "open 0xd\n", 1},
		{"non-decimal errno", "open x\n", 1},
		{"no errno", "open\n", 1},
		{"two spaces", "open  13\n", 1},
		{"trailing garbage", "open 13 Permission denied\n", 1},
		{"trailing space", "open 13 \n", 1},
		{"carriage return", "open 13\r\n", 1},
		{"NUL", "open 13\x00\n", 1},
		{"exit 0 with a failure step", "open 13\n", 0},
		{"exit 0 with mkdir", "mkdir 13\n", 0},
		{"exit 1 with ok", "ok 0\n", 1},
		{"exit 1 with mkdir-created", "mkdir-created 0\n", 1},
		{"exit 1 with relabel", "relabel 13\n", 1},
		{"exit 2", "open 13\n", 2},
		{"exit 2 with ok", "ok 0\n", 2},
		{"exit 3", "ok 0\n", 3},
		{"signal", "ok 0\n", -1},
	}
	for _, tc := range tests {
		if r, ok := ParseResult([]byte(tc.out), tc.exit); ok {
			t.Errorf("%s: ParseResult(%q, %d) accepted %+v", tc.name, tc.out, tc.exit, r)
		}
	}
}
