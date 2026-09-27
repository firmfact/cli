package update

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeReleaseEnv, when set, makes the test binary act as a downloaded
// release answering --version instead of running the tests, so the trial
// run can start a real program on every platform. Its value is the version
// to report, or "hang" or "crash".
const fakeReleaseEnv = "FIRMFACT_TEST_FAKE_RELEASE"

func TestMain(m *testing.M) {
	if behaviour, ok := os.LookupEnv(fakeReleaseEnv); ok {
		os.Exit(fakeRelease(behaviour))
	}
	os.Exit(m.Run())
}

func fakeRelease(behaviour string) int {
	if len(os.Args) != 2 || os.Args[1] != "--version" {
		fmt.Fprintf(os.Stderr, "run with %q, want only --version\n", os.Args[1:])
		return 3
	}
	if os.Getenv("FIRMFACT_NO_UPDATE_CHECK") == "" {
		fmt.Fprintln(os.Stderr, "run without FIRMFACT_NO_UPDATE_CHECK")
		return 3
	}
	switch behaviour {
	case "hang":
		time.Sleep(time.Minute)
	case "crash":
		fmt.Fprintln(os.Stderr, "cannot find a library")
		return 2
	default:
		// As cobra prints it, under the temporary file's name.
		fmt.Printf("%s version %s\n", strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe"), behaviour)
	}
	return 0
}

// ownBinary is the test binary itself, which acts as a release when
// fakeReleaseEnv says so.
func ownBinary(t *testing.T) []byte {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The new binary replaces the old one only when it runs here and reports
// the version it was downloaded as; otherwise the old one stays, untouched
// and alone.
func TestReplaceRunsTheNewBinaryFirst(t *testing.T) {
	binary := ownBinary(t)
	// A test binary built with -race otherwise idles for a second before
	// it exits.
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	cases := []struct {
		name, behaviour string
		binary          []byte
		want            string // in the error; empty for success
	}{
		{name: "the expected version", behaviour: "1.2.3", binary: binary},
		{name: "another version", behaviour: "9.9.9", binary: binary, want: `reports version "9.9.9", not 1.2.3`},
		{name: "a crash", behaviour: "crash", binary: binary, want: "cannot find a library"},
		{name: "not a program", behaviour: "1.2.3", binary: []byte("not a program"), want: "does not run here"},
		{name: "no answer", behaviour: "hang", binary: binary, want: "did not answer --version"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(fakeReleaseEnv, c.behaviour)
			prev := trialTimeout
			if c.behaviour == "hang" {
				trialTimeout = 300 * time.Millisecond
			}
			t.Cleanup(func() { trialTimeout = prev })

			target := scratchBinary(t)
			err := replace(context.Background(), target, c.binary, "1.2.3")
			if c.want == "" {
				if err != nil {
					t.Fatalf("replace: %v", err)
				}
				if got, _ := os.ReadFile(target); !bytes.Equal(got, c.binary) {
					t.Error("the target does not hold the new binary")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "not installing") {
				t.Fatalf("want an error containing %q and saying nothing was installed, got %v", c.want, err)
			}
			unchanged(t, target)
		})
	}
}

// moveAsideSwap makes the swap work as it does on Windows, where the old
// binary is renamed away before the new one takes its name.
func moveAsideSwap(t *testing.T) {
	t.Helper()
	prev := moveAside
	moveAside = true
	t.Cleanup(func() { moveAside = prev })
}

// failRenames makes rename fail with the error fail returns for a move,
// when that is not nil.
func failRenames(t *testing.T, fail func(from, to string) error) {
	t.Helper()
	prev := rename
	rename = func(from, to string) error {
		if err := fail(from, to); err != nil {
			return err
		}
		return os.Rename(from, to)
	}
	t.Cleanup(func() { rename = prev })
}

func skipTrialRun(t *testing.T) {
	t.Helper()
	prev := trialRun
	trialRun = func(context.Context, string, string) error { return nil }
	t.Cleanup(func() { trialRun = prev })
}

func TestSwapMovesTheOldBinaryAside(t *testing.T) {
	moveAsideSwap(t)
	skipTrialRun(t)
	target := scratchBinary(t)
	if err := replace(context.Background(), target, []byte("new binary"), "1.2.3"); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "new binary" {
		t.Errorf("target = %q", got)
	}
	if got, _ := os.ReadFile(oldPath(target)); string(got) != "old binary" {
		t.Errorf("%s = %q, want the old binary", oldPath(target), got)
	}
}

// Once the old binary has been moved aside, a new one that cannot take its
// place must not leave the user with no binary at all.
func TestSwapPutsTheOldBinaryBack(t *testing.T) {
	moveAsideSwap(t)
	skipTrialRun(t)
	target := scratchBinary(t)
	errInUse := errors.New("the file is in use")
	failRenames(t, func(from, to string) error {
		if to == target && from != oldPath(target) {
			return errInUse
		}
		return nil
	})
	err := replace(context.Background(), target, []byte("new binary"), "1.2.3")
	if !errors.Is(err, errInUse) || !strings.Contains(err.Error(), "the old one is back") {
		t.Fatalf("want the failed rename and word that the old binary is back, got %v", err)
	}
	unchanged(t, target)
}

// When the old binary cannot be put back either, both failures are
// reported, with where the old binary now is.
func TestSwapReportsAFailedRollback(t *testing.T) {
	moveAsideSwap(t)
	skipTrialRun(t)
	target := scratchBinary(t)
	errInUse := errors.New("the file is in use")
	errDenied := errors.New("access is denied")
	failRenames(t, func(from, to string) error {
		switch {
		case to != target:
			return nil
		case from == oldPath(target):
			return errDenied
		default:
			return errInUse
		}
	})
	err := replace(context.Background(), target, []byte("new binary"), "1.2.3")
	if !errors.Is(err, errInUse) || !errors.Is(err, errDenied) {
		t.Fatalf("want both failures, got %v", err)
	}
	if !strings.Contains(err.Error(), oldPath(target)) {
		t.Errorf("the error does not say where the old binary is: %v", err)
	}
	if got, _ := os.ReadFile(oldPath(target)); string(got) != "old binary" {
		t.Errorf("%s = %q, want the old binary", oldPath(target), got)
	}
	if entries, _ := os.ReadDir(filepath.Dir(target)); len(entries) != 1 {
		t.Errorf("the directory holds %d entries, want only the old binary", len(entries))
	}
}

// A binary that cannot be moved aside is not replaced at all.
func TestSwapLeavesABinaryThatCannotMove(t *testing.T) {
	moveAsideSwap(t)
	skipTrialRun(t)
	target := scratchBinary(t)
	errInUse := errors.New("the file is in use")
	failRenames(t, func(from, to string) error {
		if from == target {
			return errInUse
		}
		return nil
	})
	err := replace(context.Background(), target, []byte("new binary"), "1.2.3")
	if !errors.Is(err, errInUse) || !strings.Contains(err.Error(), "cannot move") {
		t.Fatalf("want the failed rename, got %v", err)
	}
	unchanged(t, target)
}

// The next run removes what an update on Windows set aside.
func TestRemoveOldBinary(t *testing.T) {
	moveAsideSwap(t)
	exe, err := executable()
	if err != nil {
		t.Fatal(err)
	}
	old := oldPath(exe)
	if err := os.WriteFile(old, []byte("old binary"), 0o755); err != nil {
		t.Skipf("cannot write next to the test binary: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(old) })
	RemoveOldBinary()
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s is still there (%v)", old, err)
	}
	RemoveOldBinary() // nothing left to remove, and no harm done
}
