package main

import (
	"bytes"
	"strings"
	"testing"
)

// Two test binaries' view of the same two packages, as go test writes it
// with -coverpkg: every block appears once per binary.
const profile = `mode: atomic
example.com/m/a/a.go:1.1,2.1 3 0
example.com/m/a/a.go:3.1,4.1 1 0
example.com/m/b/b.go:1.1,2.1 4 2
example.com/m/b/b.go:3.1,4.1 2 0
example.com/m/a/a.go:1.1,2.1 3 5
example.com/m/a/a.go:3.1,4.1 1 0
example.com/m/b/b.go:1.1,2.1 4 0
example.com/m/b/b.go:3.1,4.1 2 0
`

func TestParseMergesTheBinaries(t *testing.T) {
	cov, err := parse(strings.NewReader(profile))
	if err != nil {
		t.Fatal(err)
	}
	// A block covered by either binary counts once, not once per binary.
	if got := cov["example.com/m/a"]; got != (counts{statements: 4, covered: 3}) {
		t.Errorf("a = %+v", got)
	}
	if got := cov["example.com/m/b"]; got != (counts{statements: 6, covered: 4}) {
		t.Errorf("b = %+v", got)
	}
}

func TestParseRejectsWhatIsNotAProfile(t *testing.T) {
	for _, in := range []string{"", "mode: set\n", "mode: set\nnot a block\n", "mode: set\nx.go:1.1,2.1 three 1\n"} {
		if _, err := parse(strings.NewReader(in)); err == nil {
			t.Errorf("parse(%q) succeeded", in)
		}
	}
}

func TestCheck(t *testing.T) {
	cov, err := parse(strings.NewReader(profile))
	if err != nil {
		t.Fatal(err)
	}
	floors := func(args ...string) floorFlag {
		f := floorFlag{}
		for _, a := range args {
			if err := f.Set(a); err != nil {
				t.Fatal(err)
			}
		}
		return f
	}

	// a is at 75%, b at 66.7% and the total at 70%.
	var out bytes.Buffer
	if failures := check(&out, cov, "example.com/m", 70, floors("./a/=75", "b=60")); len(failures) != 0 {
		t.Errorf("failures = %q", failures)
	}
	for _, want := range []string{"a                                  75.0%  (floor 75%)\n", "b                                  66.7%  (floor 60%)\n", "total                              70.0%  (floor 70%)\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}

	failures := check(&bytes.Buffer{}, cov, "example.com/m", 71, floors("a=80", "gone=10"))
	want := []string{
		"a is at 75.0%, below its floor of 80%",
		"total is at 70.0%, below its floor of 71%",
		"a floor names gone, which the profile does not cover",
	}
	if strings.Join(failures, "|") != strings.Join(want, "|") {
		t.Errorf("failures = %q\nwant       %q", failures, want)
	}
}

func TestFloorFlagRejectsNonsense(t *testing.T) {
	for _, v := range []string{"internal/auth", "=80", "internal/auth=lots", "internal/auth=101"} {
		if err := (floorFlag{}).Set(v); err == nil {
			t.Errorf("Set(%q) succeeded", v)
		}
	}
}
