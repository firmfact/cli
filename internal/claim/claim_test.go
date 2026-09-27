package claim

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testEnv plays out claim on a Unix machine. That takes symlinks, execute
// bits and colon-separated PATHs, which a Windows host does not have, so
// there the test is skipped; on Windows claim writes a .cmd shim instead,
// which TestWindowsShim covers on every host.
func testEnv(t *testing.T, shell string, resolve func(string, string) (string, string, error)) *Env {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("plays out claim on a Unix machine")
	}
	home := t.TempDir()
	// Undo also looks at the fish and zsh rc files these point at, which
	// must never be the real ones.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("ZDOTDIR", "")
	self := filepath.Join(t.TempDir(), "firmfact")
	if err := os.WriteFile(self, []byte("#!/bin/sh\necho FIRMFACT \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Env{Home: home, Shell: shell, Self: self, GOOS: "linux", Path: "/usr/bin:/bin", Resolve: resolve}
}

func TestClaimFreeNameOnlyLinksAndAddsPath(t *testing.T) {
	env := testEnv(t, "bash", answer("", ""))
	p, err := Prepare(env, "ff", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.RCBlock, "export PATH=") || strings.Contains(p.RCBlock, "unalias") {
		t.Fatalf("block = %q", p.RCBlock)
	}
	if err := Apply(env, p); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(env.Home, ".local", "bin", "ff")); err != nil || target != env.Self {
		t.Fatalf("link = %q, %v", target, err)
	}
}

func TestRefusesSomeoneElsesFilesAndPrograms(t *testing.T) {
	env := testEnv(t, "bash", answer("", ""))
	link := filepath.Join(env.Home, ".local", "bin", "ff")
	must(t, os.MkdirAll(filepath.Dir(link), 0o755))
	must(t, os.WriteFile(link, []byte("not ours"), 0o755))
	if _, err := Prepare(env, "ff", ""); err == nil {
		t.Error("an existing foreign file must be refused")
	}

	env = testEnv(t, "bash", answer("file", "/usr/bin/ff"))
	if _, err := Prepare(env, "ff", ""); err == nil || !strings.Contains(err.Error(), "already a program") {
		t.Errorf("another program on PATH must be refused, got %v", err)
	}

	env = testEnv(t, "bash", answer("builtin", ""))
	if _, err := Prepare(env, "cd", ""); err == nil {
		t.Error("a builtin must be refused")
	}
	if _, err := Prepare(env, "Bad Name", ""); err == nil {
		t.Error("invalid names must be refused")
	}
}

func TestUndoRemovesExactlyWhatWasAdded(t *testing.T) {
	env := testEnv(t, "bash", answer("alias", "fzf"))
	rc := filepath.Join(env.Home, ".bashrc")
	original := "# my stuff\nalias ll='ls -l'\n"
	must(t, os.WriteFile(rc, []byte(original), 0o600))

	p, err := Prepare(env, "ff", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(env, p); err != nil {
		t.Fatal(err)
	}
	if err := Apply(env, p); err != nil { // idempotent: one block, not two
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(rc)
	if strings.Count(string(raw), markerStart("ff")) != 1 {
		t.Fatalf("expected one block:\n%s", raw)
	}
	if info, _ := os.Stat(rc); info.Mode().Perm() != 0o600 {
		t.Errorf("rc file mode changed to %v", info.Mode().Perm())
	}
	if matches, _ := filepath.Glob(rc + ".before-firmfact-claim-*"); len(matches) == 0 {
		t.Error("backup missing")
	}

	done, err := Undo(env, "ff")
	if err != nil || len(done) != 2 {
		t.Fatalf("undo = %v, %v", done, err)
	}
	raw, _ = os.ReadFile(rc)
	if strings.TrimRight(string(raw), "\n") != strings.TrimRight(original, "\n") {
		t.Fatalf("rc after undo:\n%q\nwant\n%q", raw, original)
	}
	if _, err := os.Lstat(filepath.Join(env.Home, ".local", "bin", "ff")); err == nil {
		t.Error("link not removed")
	}
}

func TestUpgradesAnOlderBlock(t *testing.T) {
	env := testEnv(t, "bash", answer("file", ""))
	env.Path = "/usr/bin:/bin:" + filepath.Join(env.Home, ".local", "bin")
	must(t, os.MkdirAll(filepath.Join(env.Home, ".local", "bin"), 0o755))
	must(t, os.Symlink(env.Self, filepath.Join(env.Home, ".local", "bin", "ff")))
	env.Resolve = answer("file", filepath.Join(env.Home, ".local", "bin", "ff"))
	v1 := setupRC("bash", "ff") + "\n" + markerStart("ff") + "\n# v1\nif [[ -n ${BASH_ALIASES[ff]+set} ]]; then alias ff-previous=\"${BASH_ALIASES[ff]}\"; unalias ff; fi\n" + markerEnd("ff") + "\n"
	rc := filepath.Join(env.Home, ".bashrc")
	must(t, os.WriteFile(rc, []byte(v1), 0o644))

	p, err := Prepare(env, "ff", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.RCFile == "" || !strings.Contains(p.RCBlock, "__firmfact_ff_block="+blockVersion) {
		t.Fatalf("expected the old block to be replaced, plan block:\n%s", p.RCBlock)
	}
	if err := Apply(env, p); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(rc)
	if strings.Count(string(raw), markerStart("ff")) != 1 || strings.Contains(string(raw), "# v1") {
		t.Fatalf("rc after upgrade:\n%s", raw)
	}
	// A second claim finds the current block and does nothing.
	env.Resolve = answer("function", "")
	p2, err := Prepare(env, "ff", "")
	if err != nil || !p2.NothingToDo() {
		t.Fatalf("second claim: %+v %v", p2, err)
	}
}

// A dotfile manager (stow, chezmoi) links ~/.bashrc to a file in its own
// tree. Claim writes that file and leaves the link a link, and so does Undo.
func TestSymlinkedRCKeepsItsLink(t *testing.T) {
	env := testEnv(t, "bash", answer("alias", "fzf"))
	dotfiles := t.TempDir()
	target := filepath.Join(dotfiles, "bashrc")
	original := "# my stuff\n"
	must(t, os.WriteFile(target, []byte(original), 0o640))
	rc := filepath.Join(env.Home, ".bashrc")
	must(t, os.Symlink(target, rc))

	p, err := Prepare(env, "ff", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.RCFile != rc || p.RCManual != "" || !strings.Contains(strings.Join(p.Notes, " "), "goes into "+target) {
		t.Fatalf("plan = %+v", p)
	}
	must(t, Apply(env, p))
	assertLinkTo := func(when string) {
		t.Helper()
		if dest, err := os.Readlink(rc); err != nil || dest != target {
			t.Fatalf("%s: %s is no longer a link to %s (%q, %v)", when, rc, target, dest, err)
		}
	}
	assertLinkTo("after claim")
	raw, _ := os.ReadFile(target)
	if !strings.HasPrefix(string(raw), original) || !strings.Contains(string(raw), markerStart("ff")) {
		t.Fatalf("target after claim:\n%s", raw)
	}
	if info, _ := os.Stat(target); info.Mode().Perm() != 0o640 {
		t.Errorf("target mode changed to %v", info.Mode().Perm())
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dotfiles, ".*firmfact-*")); len(leftovers) > 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}

	if _, err := Undo(env, "ff"); err != nil {
		t.Fatal(err)
	}
	assertLinkTo("after undo")
	if raw, _ := os.ReadFile(target); strings.TrimRight(string(raw), "\n") != strings.TrimRight(original, "\n") {
		t.Fatalf("target after undo: %q", raw)
	}
}

// A link to a file that does not exist is left alone: the block is shown
// for the user to add wherever their dotfiles live.
func TestDanglingRCLinkIsLeftToTheUser(t *testing.T) {
	env := testEnv(t, "bash", answer("alias", "fzf"))
	rc := filepath.Join(env.Home, ".bashrc")
	missing := filepath.Join(t.TempDir(), "gone", "bashrc")
	must(t, os.Symlink(missing, rc))

	p, err := Prepare(env, "ff", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.RCManual, "does not exist") || p.RCBlock == "" {
		t.Fatalf("plan = %+v", p)
	}
	must(t, Apply(env, p))
	if dest, err := os.Readlink(rc); err != nil || dest != missing {
		t.Errorf("link changed: %q, %v", dest, err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("claim created %s: %v", missing, err)
	}
	if _, err := os.Readlink(p.LinkPath); err != nil {
		t.Errorf("the command link was not made: %v", err)
	}
}

// bash on macOS runs as a login shell, which reads ~/.bash_profile (or
// ~/.bash_login, or ~/.profile), not ~/.bashrc where the block goes. The
// plan says so, and how to load ~/.bashrc, unless the login file does;
// elsewhere, and for other shells, nothing is said.
func TestMacOSBashLoginShellNote(t *testing.T) {
	has := func(p *Plan, text string) bool {
		for _, n := range p.Notes {
			if strings.Contains(n, text) {
				return true
			}
		}
		return false
	}
	for _, c := range []struct {
		goos, shell string
		files       map[string]string
		want        string // empty for no note
	}{
		{"darwin", "bash", nil, "You have no ~/.bash_profile, so the block takes effect once you make one with this line: [ -r ~/.bashrc ] && . ~/.bashrc"},
		{"darwin", "bash", map[string]string{".bash_profile": "export EDITOR=vi\n# . ~/.bashrc\n"},
			".bash_profile does not load ~/.bashrc. Should it define ff itself, move that into ~/.bashrc and claim again. The block takes effect once you add this line to the end of .bash_profile: [ -r ~/.bashrc ] && . ~/.bashrc"},
		{"darwin", "bash", map[string]string{".bash_profile": "[ -f ~/.bashrc ] && source ~/.bashrc\n"}, ""},
		// bash reads the first login file there is, and no other.
		{"darwin", "bash", map[string]string{".bash_login": "export EDITOR=vi\n", ".profile": ". ~/.bashrc\n"}, "and .bash_login does not load ~/.bashrc"},
		{"darwin", "bash", map[string]string{".profile": "if [ -n \"$BASH_VERSION\" ]; then . \"$HOME/.bashrc\"; fi\n"}, ""},
		{"linux", "bash", nil, ""},
		{"darwin", "zsh", nil, ""},
	} {
		env := testEnv(t, c.shell, answer("", ""))
		env.GOOS = c.goos
		for name, content := range c.files {
			if err := os.WriteFile(filepath.Join(env.Home, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		p, err := Prepare(env, "ff", "")
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case c.want == "" && has(p, "login shell"):
			t.Errorf("%s %s %v: notes %q", c.goos, c.shell, c.files, p.Notes)
		case c.want != "" && !has(p, c.want):
			t.Errorf("%s %s %v: notes %q, want one with %q", c.goos, c.shell, c.files, p.Notes, c.want)
		}
	}
}

// The PATH line carries the home directory. A ", a $(...) or a backtick in
// its name must neither break the rc file nor run at every shell start.
func TestPathLineQuotesAnOddHome(t *testing.T) {
	env := testEnv(t, "bash", answer("", ""))
	env.Home = filepath.Join(env.Home, "we\"ird $(touch dollar-ran) `touch tick-ran` it's")
	must(t, os.MkdirAll(env.Home, 0o755))
	binDir := filepath.Join(env.Home, ".local", "bin")
	p, err := Prepare(env, "ff", "")
	if err != nil {
		t.Fatal(err)
	}
	must(t, Apply(env, p))
	rc := filepath.Join(env.Home, ".bashrc")

	for _, shell := range []string{"bash", "zsh"} {
		t.Run(shell, func(t *testing.T) {
			if _, err := exec.LookPath(shell); err != nil {
				t.Skipf("%s not installed", shell)
			}
			if out, err := exec.Command(shell, "-n", rc).CombinedOutput(); err != nil {
				t.Fatalf("%s -n: %v\n%s", shell, err, out)
			}
			// Sourced twice: the directory goes on the PATH once.
			work := t.TempDir()
			cmd := exec.Command(shell, "-c", `PATH=/usr/bin:/bin; . "$1"; . "$1"; printf '%s' "$PATH"`, shell, rc)
			if shell == "zsh" {
				cmd.Args = append([]string{shell, "-f"}, cmd.Args[1:]...)
			}
			cmd.Dir = work
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("sourcing the rc: %v", err)
			}
			if want := "/usr/bin:/bin:" + binDir; string(out) != want {
				t.Errorf("PATH = %q\nwant   %q", out, want)
			}
			if ran, _ := os.ReadDir(work); len(ran) > 0 {
				t.Errorf("the rc file ran a command from the home directory's name: %v", ran)
			}
		})
	}
}

// Claiming again once the block is in place changes the rc file only if
// the block would change, and keeps the PATH line an earlier claim added
// even though the PATH now has the directory, thanks to that very line.
func TestReclaimKeepsTheBlockAndItsPathLine(t *testing.T) {
	env := testEnv(t, "bash", answer("alias", "fzf"))
	p, err := Prepare(env, "ff", "")
	if err != nil {
		t.Fatal(err)
	}
	must(t, Apply(env, p))
	must(t, os.Remove(p.LinkPath))
	env.Path += ":" + filepath.Join(env.Home, ".local", "bin")
	env.Resolve = answer("function", "")

	p2, err := Prepare(env, "ff", "")
	if err != nil {
		t.Fatal(err)
	}
	if p2.RCFile != "" || p2.LinkExists {
		t.Fatalf("want only the link again, plan = %+v", p2)
	}

	// An older block with a PATH line is replaced by a current one that
	// still has it.
	rc := filepath.Join(env.Home, ".bashrc")
	raw, _ := os.ReadFile(rc)
	must(t, os.WriteFile(rc, []byte(strings.Replace(string(raw), "_block="+blockVersion, "_block=1", 1)), 0o644))
	p3, err := Prepare(env, "ff", "")
	if err != nil {
		t.Fatal(err)
	}
	if p3.RCFile == "" || !strings.Contains(p3.RCBlock, "export PATH=") {
		t.Fatalf("the rewritten block lost its PATH line:\n%s", p3.RCBlock)
	}
}

// On Windows claim writes a .cmd shim into WindowsApps, which is on the
// PATH by default. It leaves a shim it did not write alone, and Undo
// removes only its own.
func TestWindowsShim(t *testing.T) {
	apps := filepath.Join(t.TempDir(), "Microsoft", "WindowsApps")
	self := filepath.Join(t.TempDir(), "firmfact.exe")
	env := &Env{Self: self, GOOS: "windows", LocalAppData: filepath.Dir(filepath.Dir(apps)), Path: apps}

	p, err := Prepare(env, "ff", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(env, p); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(apps, "ff.cmd")
	if raw, err := os.ReadFile(shim); err != nil || string(raw) != "@rem firmfact claim ff\r\n@\""+self+"\" %*\r\n" {
		t.Fatalf("shim = %q, %v", raw, err)
	}
	if p, err := Prepare(env, "ff", ""); err != nil || !p.NothingToDo() {
		t.Errorf("a second claim: %+v, %v", p, err)
	}

	must(t, os.WriteFile(filepath.Join(apps, "fg.cmd"), []byte("@echo someone else's\r\n"), 0o644))
	if _, err := Prepare(env, "fg", ""); err == nil || !strings.Contains(err.Error(), "was not written by firmfact") {
		t.Errorf("someone else's shim: %v", err)
	}
	if done, err := Undo(env, "fg"); err != nil || len(done) != 0 {
		t.Errorf("undo of someone else's shim = %v, %v", done, err)
	}
	if done, err := Undo(env, "ff"); err != nil || len(done) != 1 {
		t.Errorf("undo = %v, %v", done, err)
	}
	if _, err := os.Stat(shim); !os.IsNotExist(err) {
		t.Errorf("shim still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(apps, "fg.cmd")); err != nil {
		t.Errorf("someone else's shim went: %v", err)
	}
}

// cmd.exe expands %VAR% and !VAR! inside the shim's quotes, and a " ends
// them, so a firmfact path with any of those gets no shim.
func TestWindowsShimRefusesAPathItCannotQuote(t *testing.T) {
	apps := filepath.Join(t.TempDir(), "Microsoft", "WindowsApps")
	for _, dir := range []string{`100%USERNAME%`, `say "hi"`, `wow!`} {
		self := filepath.Join(t.TempDir(), dir, "firmfact.exe")
		env := &Env{Self: self, GOOS: "windows", LocalAppData: filepath.Dir(filepath.Dir(apps)), Path: apps}
		if _, err := Prepare(env, "ff", ""); err == nil || !strings.Contains(err.Error(), "cannot safely run") {
			t.Errorf("%s: want a refusal, got %v", self, err)
		}
	}
}

func TestRefusesWhenAnotherProgramComesFirstOnPath(t *testing.T) {
	env := testEnv(t, "bash", answer("alias", "fzf"))
	early := t.TempDir()
	must(t, os.WriteFile(filepath.Join(early, "ff"), []byte("#!/bin/sh\n"), 0o755))
	env.Path = early + ":" + filepath.Join(env.Home, ".local", "bin")
	if _, err := Prepare(env, "ff", ""); err == nil || !strings.Contains(err.Error(), "already a program") {
		t.Fatalf("expected a refusal, got %v", err)
	}
}

// answer is a Resolve that reports name as kind, without asking a shell.
func answer(kind, detail string) func(string, string) (string, string, error) {
	return func(string, string) (string, string, error) { return kind, detail, nil }
}

// must stops a test whose setup failed, so that no later assertion fails
// for a reason that has nothing to do with it.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
