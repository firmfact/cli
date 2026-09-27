// Command covergate fails CI when test coverage drops below its floors:
//
//	go test -coverpkg=./... -coverprofile=cover.out ./...
//	go run ./internal/covergate -profile cover.out -min 70 -floor internal/auth=85
//
// It prints the coverage of every package, so a failing run shows where the
// tests went missing.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
)

func main() {
	profile := flag.String("profile", "cover.out", "coverage profile written by go test -coverprofile")
	minTotal := flag.Float64("min", 0, "lowest acceptable coverage across all packages, in per cent")
	floors := floorFlag{}
	flag.Var(floors, "floor", "lowest acceptable coverage of one package, as `dir=percent` (repeatable)")
	flag.Parse()

	f, err := os.Open(*profile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "covergate:", err)
		os.Exit(2)
	}
	cov, err := parse(f)
	_ = f.Close() // only read
	if err != nil {
		fmt.Fprintln(os.Stderr, "covergate:", err)
		os.Exit(2)
	}
	if failures := check(os.Stdout, cov, modulePath(), *minTotal, floors); len(failures) > 0 {
		fmt.Fprintln(os.Stderr)
		for _, msg := range failures {
			fmt.Fprintln(os.Stderr, "covergate:", msg)
		}
		os.Exit(1)
	}
}

// counts is the number of statements in a package and how many of them ran.
type counts struct{ statements, covered int }

func (c counts) percent() float64 {
	if c.statements == 0 {
		return 100
	}
	return 100 * float64(c.covered) / float64(c.statements)
}

// parse reads a profile into counts per package import path. With
// -coverpkg every test binary reports every package, so each block appears
// once per binary; a block counts as covered when any test ran it, which is
// how `go tool cover` merges them too.
func parse(r io.Reader) (map[string]counts, error) {
	type block struct {
		statements int
		covered    bool
	}
	blocks := map[string]*block{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || (n == 1 && strings.HasPrefix(line, "mode:")) {
			continue
		}
		// file.go:12.2,14.16 3 1: a block, its statements and its count.
		fields := strings.Fields(line)
		if len(fields) != 3 || !strings.Contains(fields[0], ":") {
			return nil, fmt.Errorf("line %d is not a coverage block: %q", n, line)
		}
		statements, err1 := strconv.Atoi(fields[1])
		count, err2 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("line %d is not a coverage block: %q", n, line)
		}
		b := blocks[fields[0]]
		if b == nil {
			b = &block{statements: statements}
			blocks[fields[0]] = b
		}
		b.covered = b.covered || count > 0
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(blocks) == 0 {
		return nil, errors.New("the profile holds no coverage blocks")
	}
	cov := map[string]counts{}
	for pos, b := range blocks {
		file := pos[:strings.LastIndex(pos, ":")]
		pkg := path.Dir(file)
		c := cov[pkg]
		c.statements += b.statements
		if b.covered {
			c.covered += b.statements
		}
		cov[pkg] = c
	}
	return cov, nil
}

// check writes the report to w and returns what fell short. Packages are
// shown relative to module, and floors name them the same way.
func check(w io.Writer, cov map[string]counts, module string, minTotal float64, floors floorFlag) []string {
	rel := func(pkg string) string {
		if pkg == module {
			return "."
		}
		return strings.TrimPrefix(pkg, module+"/")
	}
	var pkgs []string
	var total counts
	byDir := map[string]counts{}
	for pkg, c := range cov {
		pkgs = append(pkgs, rel(pkg))
		byDir[rel(pkg)] = c
		total.statements += c.statements
		total.covered += c.covered
	}
	sort.Strings(pkgs)

	var failures []string
	line := func(name string, c counts, floor float64, hasFloor bool) {
		fmt.Fprintf(w, "%-32s %6.1f%%", name, c.percent())
		if hasFloor {
			fmt.Fprintf(w, "  (floor %g%%)", floor)
			if c.percent() < floor {
				fmt.Fprint(w, "  BELOW")
				failures = append(failures, fmt.Sprintf("%s is at %.1f%%, below its floor of %g%%", name, c.percent(), floor))
			}
		}
		fmt.Fprintln(w)
	}
	for _, dir := range pkgs {
		floor, ok := floors[dir]
		line(dir, byDir[dir], floor, ok)
	}
	line("total", total, minTotal, minTotal > 0)

	var unknown []string
	for dir := range floors {
		if _, ok := byDir[dir]; !ok {
			unknown = append(unknown, dir)
		}
	}
	sort.Strings(unknown)
	for _, dir := range unknown {
		// A renamed package must not quietly lose its floor.
		failures = append(failures, fmt.Sprintf("a floor names %s, which the profile does not cover", dir))
	}
	return failures
}

// modulePath is the module in go.mod in the current directory, or "" when
// there is none, in which case packages keep their full import paths.
func modulePath() string {
	raw, err := os.ReadFile("go.mod")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	return ""
}

// floorFlag collects -floor dir=percent.
type floorFlag map[string]float64

func (f floorFlag) String() string { return "" }

func (f floorFlag) Set(v string) error {
	dir, pct, ok := strings.Cut(v, "=")
	n, err := strconv.ParseFloat(pct, 64)
	if !ok || dir == "" || err != nil || n < 0 || n > 100 {
		return fmt.Errorf("want dir=percent, such as internal/auth=85, not %q", v)
	}
	f[strings.TrimSuffix(strings.TrimPrefix(dir, "./"), "/")] = n
	return nil
}
