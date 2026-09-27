//go:build !windows

package claim

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// playUser makes the test run as if the user had these ids, so files the
// test created belong to someone else without needing root.
func playUser(t *testing.T, uid int, gids []int) {
	t.Helper()
	prev := currentIDs
	currentIDs = func() (int, []int) { return uid, gids }
	t.Cleanup(func() { currentIDs = prev })
}

// Replacing an rc file by renaming over it would make the user the owner
// of a file that belongs to root or another user, or move it out of its
// group, so claim shows the block to add by hand instead. It still makes
// the link, and Undo says which lines to remove.
func TestRCOwnedBySomeoneElseIsLeftToTheUser(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		play       func(t *testing.T)
	}{
		{"another user", "belongs to", func(t *testing.T) { playUser(t, os.Getuid()+1, []int{os.Getgid()}) }},
		{"a group the user is not in", "which you are not in", func(t *testing.T) { playUser(t, os.Getuid(), []int{os.Getgid() + 1}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t, "bash", answer("alias", "fzf"))
			rc := filepath.Join(env.Home, ".bashrc")
			original := "# managed elsewhere\n"
			must(t, os.WriteFile(rc, []byte(original), 0o644))
			tc.play(t)

			p, err := Prepare(env, "ff", "")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(p.RCManual, tc.want) || !strings.Contains(p.RCBlock, markerStart("ff")) {
				t.Fatalf("plan = %+v", p)
			}
			must(t, Apply(env, p))
			if raw, _ := os.ReadFile(rc); string(raw) != original {
				t.Errorf("rc changed:\n%s", raw)
			}
			if backups, _ := filepath.Glob(rc + ".before-firmfact-claim-*"); len(backups) > 0 {
				t.Errorf("backup written for a file claim did not change: %v", backups)
			}
			if _, err := os.Readlink(p.LinkPath); err != nil {
				t.Errorf("the command link was not made: %v", err)
			}

			// The user pasted the block; Undo leaves it and says so.
			pasted := original + "\n" + p.RCBlock
			must(t, os.WriteFile(rc, []byte(pasted), 0o644))
			done, err := Undo(env, "ff")
			if err == nil || !strings.Contains(err.Error(), "remove the lines") || len(done) != 1 {
				t.Fatalf("undo = %v, %v", done, err)
			}
			if raw, _ := os.ReadFile(rc); string(raw) != pasted {
				t.Errorf("undo changed the rc:\n%s", raw)
			}
		})
	}
}

// probeHome is a home directory whose .bashrc is rc, as the shell probe
// sees it.
func probeHome(t *testing.T, rc string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	home := t.TempDir()
	must(t, os.WriteFile(filepath.Join(home, ".bashrc"), []byte(rc), 0o644))
	t.Setenv("HOME", home)
	prev := probeTimeout
	probeTimeout = 500 * time.Millisecond
	t.Cleanup(func() { probeTimeout = prev })
	return home
}

// An rc file that waits for input would hold the probe, and claim with it,
// for ever; the probe gives up and claim says why.
func TestProbeGivesUpOnAnRCThatWaitsForInput(t *testing.T) {
	home := t.TempDir()
	fifo := filepath.Join(home, "fifo")
	must(t, syscall.Mkfifo(fifo, 0o600))
	home = probeHome(t, `read -r line < "`+fifo+`"`+"\n")

	start := time.Now()
	_, _, err := resolveInShell("bash", "ff")
	if err == nil || !strings.Contains(err.Error(), "did not finish within") {
		t.Fatalf("want a timeout, got %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the probe took %s", took)
	}

	env := testEnv(t, "bash", resolveInShell)
	env.Home = home
	if _, err := Prepare(env, "ff", ""); err == nil || !strings.Contains(err.Error(), "could not inspect your shell") || !strings.Contains(err.Error(), "--shell") {
		t.Fatalf("Prepare = %v", err)
	}
}

// A program an rc file leaves running in the background keeps the probe's
// output open; the answer the shell gave still counts, without waiting for
// that program to end.
func TestProbeAnswersDespiteABackgroundProgram(t *testing.T) {
	probeHome(t, "alias ff=fzf\nsleep 8 &\n")
	start := time.Now()
	kind, detail, err := resolveInShell("bash", "ff")
	if err != nil || kind != "alias" || detail != "fzf" {
		t.Fatalf("resolve = %q %q %v", kind, detail, err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the probe waited %s for the background program", took)
	}
}

// A shell that is not installed cannot be asked; claim says so rather than
// assume the name is free.
func TestProbeReportsAMissingShell(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, _, err := resolveInShell("zsh", "ff"); err == nil {
		t.Fatal("want an error for a shell that is not installed")
	}
}
