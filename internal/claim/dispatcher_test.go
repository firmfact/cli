package claim

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests claim a name in a real bash, zsh and fish, and run it. Each
// skips a shell that is not installed; CONTRIBUTING.md says how to run them
// in containers with every shell, bash 3.2 (macOS's /bin/bash) among them.
var dispatcherShells = []string{"bash", "zsh", "fish"}

// omarchyRC reproduces Omarchy's ff setup: an alias, a helper alias that
// calls it inside $(...), and a helper function that pipes into it inside
// $(...). prevff stands in for fzf and shows its arguments and stdin. NAME
// is the name claimed.
const omarchyRC = `prevff() { printf 'FZF args=[%s] stdin=[%s]\n' "$*" "$(cat)"; }
alias NAME='prevff'
alias eff='echo EDIT "$(NAME)"'
sff() { local file; file=$(printf 'pick' | NAME) && echo "SCP $file"; }
`

// omarchyFishRC is the same for fish, where an alias is a function. A
// command substitution in fish does not read the function's stdin, so
// prevff reads it with read.
const omarchyFishRC = `function prevff
    read -z -l stdin
    printf 'FZF args=[%s] stdin=[%s]\n' "$argv" (string trim -- "$stdin")
end
alias NAME=prevff
function eff
    echo EDIT (NAME)
end
function sff
    set -l file (printf 'pick' | NAME); and echo "SCP $file"
end
`

// setupRC is the Omarchy setup in shell's language, for name.
func setupRC(shell, name string) string {
	if shell == "fish" {
		return strings.ReplaceAll(omarchyFishRC, "NAME", name)
	}
	return strings.ReplaceAll(omarchyRC, "NAME", name)
}

// omarchyUse runs name the ways Omarchy does and the ways firmfact is run;
// wantOmarchyUse is what each prints once name is claimed.
func omarchyUse(name string) []string {
	return []string{
		name + " hello world",
		"echo x | " + name,
		"sff",
		"eff </dev/null",
		"echo y | " + name + "-previous",
		name + " --json whoami",
	}
}

var wantOmarchyUse = []string{
	"@FIRMFACT hello world",
	"@FZF args=[] stdin=[x]",
	"@SCP FZF args=[] stdin=[pick]",
	"@EDIT FZF args=[] stdin=[]",
	"@FZF args=[] stdin=[y]",
	"@FIRMFACT --json whoami",
}

func TestDispatcherKeepsOmarchyHelpersWorking(t *testing.T) {
	for _, shell := range dispatcherShells {
		t.Run(shell, func(t *testing.T) {
			env := claimInRealShell(t, shell, "ff")
			got := atLines(shellIn(t, shell, env.Home, atScript(shell, omarchyUse("ff")...)))
			if strings.Join(got, "|") != strings.Join(wantOmarchyUse, "|") {
				t.Fatalf("got  %q\nwant %q", got, wantOmarchyUse)
			}

			// Uninstalled: with no firmfact on the PATH every call goes to the old ff.
			must(t, os.Remove(filepath.Join(env.Home, ".local", "bin", "ff")))
			must(t, os.Remove(env.Self))
			if got := atLines(shellIn(t, shell, env.Home, atScript(shell, "ff hello </dev/null"))); len(got) != 1 || got[0] != "@FZF args=[hello] stdin=[]" {
				t.Fatalf("after uninstall got %q", got)
			}
		})
	}
}

// A name may have a hyphen, which no shell allows in the name of a
// variable; the block's own variables have an underscore in its place.
func TestDispatcherForAHyphenatedName(t *testing.T) {
	for _, shell := range dispatcherShells {
		t.Run(shell, func(t *testing.T) {
			env := claimInRealShell(t, shell, "f-f")
			stdout, stderr := shellRun(t, shell, env.Home, atScript(shell, omarchyUse("f-f")...))
			if got := atLines(stdout); strings.Join(got, "|") != strings.Join(wantOmarchyUse, "|") {
				t.Fatalf("got  %q\nwant %q", got, wantOmarchyUse)
			}
			// fish runs the dispatcher even so, but complains at every start.
			if strings.Contains(stderr, "__firmfact") {
				t.Errorf("the shell complains about the block:\n%s", stderr)
			}
		})
	}
}

// A dotfile manager may wrap the block in a conditional, so the shell reads
// all of it, alias and all, before it runs any of it.
func TestBlockWorksWhenWrapped(t *testing.T) {
	for _, shell := range dispatcherShells {
		t.Run(shell, func(t *testing.T) {
			env := claimInRealShell(t, shell, "ff")
			rc, err := rcFile(env)
			must(t, err)
			raw, err := os.ReadFile(rc)
			must(t, err)
			block := extractBlock(string(raw), "ff")
			begin, end := "if true; then\n", "\nfi"
			if shell == "fish" {
				begin, end = "if true\n", "\nend"
			}
			must(t, os.WriteFile(rc, []byte(strings.Replace(string(raw), block, begin+block+end, 1)), 0o644))
			got := atLines(shellIn(t, shell, env.Home, atScript(shell, "ff a", "echo z | ff")))
			if strings.Join(got, "|") != "@FIRMFACT a|@FZF args=[] stdin=[z]" {
				t.Fatalf("wrapped block: %q", got)
			}
		})
	}
}

// bashBlockV2 is the bash block claims wrote before block version 3. It
// found aliases in BASH_ALIASES, which bash 3.2 does not have, so there it
// left the alias in place and ff never ran firmfact.
const bashBlockV2 = `# >>> firmfact claim ff >>>
# Added by ` + "`firmfact claim ff`. Remove with `firmfact claim ff --undo`." + `
__firmfact_ff_block=2
if [[ -n ${BASH_ALIASES[ff]+set} ]]; then
  __firmfact_ff_prev=${BASH_ALIASES[ff]}
  unalias ff
  function ff-previous { eval "$__firmfact_ff_prev \"\$@\""; }
elif declare -F ff >/dev/null && [[ $(declare -f ff) != *__firmfact_ff_dispatch* ]]; then
  eval "$(declare -f ff | sed '1s/^ff /ff-previous /')"
  unset -f ff
fi
if declare -F ff-previous >/dev/null; then
  function ff {
    : __firmfact_ff_dispatch
    if (( $# )) && type -P ff >/dev/null; then command ff "$@"
    elif declare -F ff-previous >/dev/null; then ff-previous "$@"
    else command ff "$@"; fi
  }
fi
# <<< firmfact claim ff <<<
`

// Claiming again replaces a version 2 block with the current one, which
// takes ff over in every bash: in bash 3.2 the shell still has the alias,
// in later ones the old dispatcher.
func TestClaimReplacesTheBlockThatMissedAliasesInBash3(t *testing.T) {
	env := realShellEnv(t, "bash")
	bin := filepath.Join(env.Home, ".local", "bin")
	must(t, os.MkdirAll(bin, 0o755))
	must(t, os.Symlink(env.Self, filepath.Join(bin, "ff")))
	rc := filepath.Join(env.Home, ".bashrc")
	must(t, os.WriteFile(rc, []byte(setupRC("bash", "ff")+"\n"+bashBlockV2), 0o644))

	p, err := Prepare(env, "ff", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.RCFile == "" || !p.TakesOver || !strings.Contains(strings.Join(p.Notes, "\n"), "replaces the block from an earlier claim") {
		t.Fatalf("plan = %+v", p)
	}
	must(t, Apply(env, p))
	raw, err := os.ReadFile(rc)
	must(t, err)
	if strings.Count(string(raw), markerStart("ff")) != 1 || strings.Contains(string(raw), "_block=2") {
		t.Fatalf("rc after claiming again:\n%s", raw)
	}
	got := atLines(shellIn(t, "bash", env.Home, atScript("bash", omarchyUse("ff")...)))
	if strings.Join(got, "|") != strings.Join(wantOmarchyUse, "|") {
		t.Fatalf("got  %q\nwant %q", got, wantOmarchyUse)
	}
}

// claimInRealShell writes the Omarchy setup for name as shell's rc file
// and claims name as `firmfact claim` does, asking the shell what name is.
func claimInRealShell(t *testing.T, shell, name string) *Env {
	t.Helper()
	env := realShellEnv(t, shell)
	rc, err := rcFile(env)
	must(t, err)
	must(t, os.MkdirAll(filepath.Dir(rc), 0o755))
	must(t, os.WriteFile(rc, []byte(setupRC(shell, name)), 0o644))
	p, err := Prepare(env, name, "")
	if err != nil {
		t.Fatal(err)
	}
	// The probe saw what the rc file defines: fish's alias is a function.
	want := "your shell defines " + name + " as an alias (prevff)"
	if shell == "fish" {
		want = "your shell defines " + name + " as a function"
	}
	if !p.TakesOver || p.Existing != want {
		t.Fatalf("plan = %+v\nwant it to take over what %s", p, want)
	}
	must(t, Apply(env, p))
	return env
}

// realShellEnv is a testEnv for shell whose Resolve asks that shell.
func realShellEnv(t *testing.T, shell string) *Env {
	t.Helper()
	if _, err := exec.LookPath(shell); err != nil {
		t.Skipf("%s not installed", shell)
	}
	env := testEnv(t, shell, nil)
	env.Path = "/usr/bin:/bin:" + filepath.Join(env.Home, ".local", "bin")
	env.Resolve = func(shell, name string) (string, string, error) {
		kind, detail := runResolve(t, env.Home, shell, name)
		return kind, detail, nil
	}
	return env
}

func runResolve(t *testing.T, home, shell, name string) (string, string) {
	t.Helper()
	cmd := exec.Command(shell, "-ic", resolveScript(shell, name))
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin:" + filepath.Join(home, ".local", "bin"), "TERM=dumb"}
	out, _ := cmd.Output()
	kind, detail, _ := parseResolve(string(out))
	return kind, detail
}

// shellIn runs script in an interactive shell, which reads the rc file in
// home, and returns what it printed.
func shellIn(t *testing.T, shell, home, script string) string {
	t.Helper()
	stdout, _ := shellRun(t, shell, home, script)
	return stdout
}

// shellRun is shellIn with stderr too, where an interactive shell without
// a terminal also says it has no job control.
func shellRun(t *testing.T, shell, home, script string) (stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(shell, "-ic", script)
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin:" + filepath.Join(home, ".local", "bin"), "TERM=dumb"}
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	out, _ := cmd.Output()
	return string(out), errBuf.String()
}

// atScript prints the output of each command on a line of its own, after
// an @.
func atScript(shell string, commands ...string) string {
	var b strings.Builder
	for _, c := range commands {
		if shell == "fish" {
			// "$o" is one word even when the command printed nothing.
			b.WriteString("set o (" + c + "); echo \"@$o\"\n")
		} else {
			b.WriteString("echo \"@$(" + c + ")\"\n")
		}
	}
	return b.String()
}

func atLines(out string) []string {
	var got []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "@") {
			got = append(got, line)
		}
	}
	return got
}
