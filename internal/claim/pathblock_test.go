package claim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A free name claimed while ~/.local/bin was not on the PATH gets a block
// that only adds the directory. Claiming it again finds nothing to do; the
// block did not take the name over, and is not made to now.
func TestReclaimOfAFreeNameKeepsItsPathBlock(t *testing.T) {
	env := testEnv(t, "bash", answer("", ""))
	p, err := Prepare(env, "ff", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.TakesOver || !strings.Contains(p.RCBlock, "export PATH=") || strings.Contains(p.RCBlock, "_block=") {
		t.Fatalf("a free name's plan: takes over %v, block\n%s", p.TakesOver, p.RCBlock)
	}
	must(t, Apply(env, p))

	// The shell now finds the link, through the PATH the block set.
	env.Resolve = answer("file", p.LinkPath)
	again, err := Prepare(env, "ff", "")
	if err != nil {
		t.Fatal(err)
	}
	if !again.NothingToDo() {
		t.Errorf("claiming again: %+v", again)
	}

	// A function the user defines later is theirs, not a dispatcher of
	// ours: claim takes it over, keeping the directory on the PATH.
	env.Resolve = answer("function", "")
	later, err := Prepare(env, "ff", "")
	if err != nil {
		t.Fatal(err)
	}
	if !later.TakesOver || later.Existing != "your shell defines ff as a function" ||
		!strings.Contains(later.RCBlock, "export PATH=") || !strings.Contains(later.RCBlock, "ff-previous") {
		t.Errorf("with a function defined since: %+v", later)
	}
}

// A PATH-only block in the form an older claim wrote is brought up to date
// as the same kind of block, with a note that it is replaced.
func TestOlderPathBlockIsRewrittenAsOne(t *testing.T) {
	env := testEnv(t, "bash", answer("", ""))
	bin := filepath.Join(env.Home, ".local", "bin")
	old := markerStart("ff") + "\n# Added by `firmfact claim ff`.\n" +
		`case ":$PATH:" in *":` + bin + `:"*) ;; *) export PATH="` + bin + `:$PATH" ;; esac` + "\n" + markerEnd("ff") + "\n"
	must(t, os.WriteFile(filepath.Join(env.Home, ".bashrc"), []byte("export KEEP=1\n\n"+old), 0o644))

	p, err := Prepare(env, "ff", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.TakesOver || p.RCFile == "" || strings.Contains(p.RCBlock, "ff-previous") || !strings.Contains(p.RCBlock, `export PATH="$PATH":`) {
		t.Fatalf("plan: takes over %v, rc %q, block\n%s", p.TakesOver, p.RCFile, p.RCBlock)
	}
	if !strings.Contains(strings.Join(p.Notes, "\n"), "This replaces the block from an earlier claim") {
		t.Errorf("notes = %q", p.Notes)
	}
}

// Each shell's rc file: bash's and zsh's in the home directory (zsh's under
// ZDOTDIR when that is set), fish's under XDG_CONFIG_HOME; any other shell
// is refused with the ones claim knows.
func TestRCFilePerShell(t *testing.T) {
	env := testEnv(t, "bash", nil)
	xdg := os.Getenv("XDG_CONFIG_HOME")
	zdot := filepath.Join(t.TempDir(), "zsh")
	cases := []struct {
		shell, zdotdir, want string
	}{
		{"bash", "", filepath.Join(env.Home, ".bashrc")},
		{"zsh", "", filepath.Join(env.Home, ".zshrc")},
		{"zsh", zdot, filepath.Join(zdot, ".zshrc")},
		{"fish", "", filepath.Join(xdg, "fish", "config.fish")},
	}
	for _, c := range cases {
		t.Setenv("ZDOTDIR", c.zdotdir)
		env.Shell = c.shell
		if got, err := rcFile(env); err != nil || got != c.want {
			t.Errorf("%s (ZDOTDIR=%q): %q, %v; want %q", c.shell, c.zdotdir, got, err, c.want)
		}
	}

	t.Setenv("XDG_CONFIG_HOME", "")
	env.Shell = "fish"
	if got, err := rcFile(env); err != nil || got != filepath.Join(env.Home, ".config", "fish", "config.fish") {
		t.Errorf("fish without XDG_CONFIG_HOME: %q, %v", got, err)
	}
	env.Shell = "tcsh"
	if _, err := rcFile(env); err == nil || !strings.Contains(err.Error(), `the "tcsh" shell; pass --shell bash, zsh or fish`) {
		t.Errorf("tcsh: %v", err)
	}
}
