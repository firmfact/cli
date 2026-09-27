// Package claim makes a short command name (by default "ff") run the
// firmfact CLI, on request and reversibly.
//
// Two parts:
//
//   - a link: ~/.local/bin/<name> pointing at this binary (on Windows a
//     <name>.cmd shim in %LOCALAPPDATA%\Microsoft\WindowsApps, which is on the
//     PATH by default);
//   - only when the shell already defines <name> as an alias or function
//     (Omarchy ships `alias ff=fzf ...`), a marked block at the end of the
//     shell's rc file that turns <name> into a small dispatcher: with
//     arguments it runs firmfact, without arguments it runs the previous
//     definition, which stays available as <name>-previous. Bare use and
//     helpers that call it in a pipe or $(...) (Omarchy's sff and eff) keep
//     working, and with no firmfact on the PATH the block steps aside.
//
// Nothing is written without the caller's confirmation, other people's files
// and programs are never overwritten, and Undo removes exactly what Claim added.
package claim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,15}$`)

// Env is everything Claim touches, so tests can point it at a temp dir.
type Env struct {
	Home         string
	Shell        string // "bash", "zsh", "fish" or "" (unknown / Windows)
	Self         string // resolved path of the firmfact binary
	GOOS         string
	Path         string // PATH value
	LocalAppData string
	// Resolve reports how the user's interactive shell sees name:
	// kind is "alias", "function", "file", "builtin", "keyword" or "";
	// detail is the alias value or the file path. err says the shell could
	// not be asked, as when its rc file waits for input.
	Resolve func(shell, name string) (kind, detail string, err error)
}

// DefaultEnv reads the real environment.
func DefaultEnv() (*Env, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &Env{
		Home:         home,
		Shell:        filepath.Base(os.Getenv("SHELL")),
		Self:         self,
		GOOS:         runtime.GOOS,
		Path:         os.Getenv("PATH"),
		LocalAppData: os.Getenv("LOCALAPPDATA"),
		Resolve:      resolveInShell,
	}, nil
}

// Plan is what Claim would do, shown to the user before anything changes.
type Plan struct {
	Name       string
	KeepAs     string // the new name for an existing alias/function, if any
	LinkPath   string
	LinkBody   string // Windows shim contents; empty for a symlink
	LinkExists bool   // an identical link is already in place
	RCFile     string // empty when no rc change is needed
	RCBlock    string
	RCManual   string // why the user adds RCBlock by hand; Apply then leaves RCFile alone
	Existing   string // human description of what currently answers to Name
	Backup     string // set by Apply: the dated copy of the rc file it changed
	Notes      []string
	// TakesOver is set when RCBlock makes Name a dispatcher that keeps the
	// previous definition as KeepAs, rather than only putting ~/.local/bin
	// on the PATH.
	TakesOver bool
}

func (p *Plan) NothingToDo() bool { return p.LinkExists && p.RCFile == "" }

func markerStart(name string) string { return "# >>> firmfact claim " + name + " >>>" }
func markerEnd(name string) string   { return "# <<< firmfact claim " + name + " <<<" }

// Prepare works out the plan for claiming name, keeping any existing
// alias or function under keepAs.
func Prepare(env *Env, name, keepAs string) (*Plan, error) {
	if !validName.MatchString(name) {
		return nil, fmt.Errorf("%q is not a usable command name", name)
	}
	if keepAs == "" {
		keepAs = name + "-previous"
	}
	if !validName.MatchString(keepAs) || keepAs == name {
		return nil, fmt.Errorf("%q is not a usable name to keep the old %s under", keepAs, name)
	}
	p := &Plan{Name: name, KeepAs: keepAs}

	if env.GOOS == "windows" {
		return prepareWindows(env, p)
	}

	binDir := filepath.Join(env.Home, ".local", "bin")
	p.LinkPath = filepath.Join(binDir, name)
	switch target, err := os.Readlink(p.LinkPath); {
	case err == nil && sameFile(target, env.Self):
		p.LinkExists = true
	case err == nil:
		if resolved, rerr := filepath.EvalSymlinks(p.LinkPath); rerr != nil || !sameFile(resolved, env.Self) {
			return nil, fmt.Errorf("%s already exists and points to %s; remove it yourself if you want to replace it", p.LinkPath, target)
		}
		p.LinkExists = true
	case errors.Is(err, os.ErrNotExist):
	default:
		if _, statErr := os.Lstat(p.LinkPath); statErr == nil {
			return nil, fmt.Errorf("%s already exists and is not a link to firmfact; remove it yourself if you want to replace it", p.LinkPath)
		}
	}

	// Another program named `name` earlier on the PATH would still win over
	// the link, whatever the shell block does.
	if first := firstOnPath(env.Path, name); first != "" && !sameFile(first, p.LinkPath) && !sameFile(first, env.Self) {
		return nil, fmt.Errorf("%s is already a program on your PATH (%s); claiming it would hide that program", name, first)
	}

	rc, rcErr := rcFile(env)
	existingBlock := ""
	if rcErr == nil {
		if raw, err := os.ReadFile(rc); err == nil {
			existingBlock = extractBlock(string(raw), name)
		}
	}

	var lines []string
	if !onPath(env.Path, binDir) {
		p.Notes = append(p.Notes, binDir+" is not on your PATH yet; the shell block adds it at the end.")
		lines = append(lines, pathLine(env.Shell, binDir))
	} else if addsPath(existingBlock) {
		// The PATH this process sees may have the directory only because an
		// earlier block put it there, so a rewritten block keeps doing so.
		lines = append(lines, pathLine(env.Shell, binDir))
	}

	kind, detail := "", ""
	if env.Resolve != nil && env.Shell != "" {
		var err error
		if kind, detail, err = env.Resolve(env.Shell, name); err != nil {
			return nil, fmt.Errorf("could not inspect your shell: %w; pass --shell if %s is not the shell you use", err, env.Shell)
		}
	}
	// Every earlier block took name over but one that only put binDir on
	// the PATH, as claiming a free name does.
	tookOver := existingBlock != "" && !onlyAddsPath(existingBlock)
	takeover := false
	switch kind {
	case "alias", "function":
		if kind == "function" && tookOver {
			break // our own dispatcher from an earlier claim
		}
		article := "a"
		if kind == "alias" {
			article = "an"
		}
		p.Existing = fmt.Sprintf("your shell defines %s as %s %s", name, article, kind)
		if detail != "" {
			p.Existing += " (" + abbreviate(detail) + ")"
		}
		takeover = true
	case "builtin", "keyword":
		return nil, fmt.Errorf("%s is a %s shell %s and cannot be claimed", name, env.Shell, kind)
	case "file":
		if !sameFile(detail, p.LinkPath) && !sameFile(detail, env.Self) {
			p.Existing = fmt.Sprintf("%s runs %s", name, detail)
			return nil, fmt.Errorf("%s is already a program on your PATH (%s); claiming it would hide that program", name, detail)
		}
	}
	// A block from an earlier claim is rewritten with the current version, so
	// fixes reach people who claimed before them. One that took nothing over
	// is not made to take over now: there is no previous name to keep.
	if tookOver {
		takeover = true
		if !blockIsCurrent(existingBlock, env.Shell, name) {
			p.Notes = append(p.Notes, "This replaces the block from an earlier claim with the current version.")
		}
	}
	if takeover {
		lines = append(lines, takeOver(env.Shell, name, keepAs)...)
		p.Notes = append(p.Notes, fmt.Sprintf("%s with arguments runs firmfact (%s whoami); %s on its own, and helpers that call it in a pipe or $(...), keep running your previous %s, which is also available as %s.",
			name, name, name, name, keepAs))
	}

	if len(lines) == 0 {
		return p, nil
	}
	block := strings.Join(append(append([]string{
		markerStart(name),
		"# Added by `firmfact claim " + name + "`. Remove with `firmfact claim " + name + " --undo`.",
	}, lines...), markerEnd(name)), "\n") + "\n"
	if strings.TrimSuffix(block, "\n") == existingBlock {
		return p, nil // the rc file already has exactly this block
	}
	if existingBlock != "" && !tookOver {
		p.Notes = append(p.Notes, "This replaces the block from an earlier claim with the current version.")
	}
	if rcErr != nil {
		return nil, rcErr
	}
	target, manual, err := rcTarget(rc)
	if err != nil {
		return nil, err
	}
	p.RCFile, p.RCBlock, p.RCManual, p.TakesOver = rc, block, manual, takeover
	if manual == "" && target != rc {
		p.Notes = append(p.Notes, fmt.Sprintf("%s is a link; the block goes into %s, and the link stays.", rc, target))
	}
	if env.GOOS == "darwin" && env.Shell == "bash" {
		if note := bashLoginNote(env.Home, name); note != "" {
			p.Notes = append(p.Notes, note)
		}
	}
	return p, nil
}

// bashLoginNote is what a bash user on macOS needs to know before the
// block in ~/.bashrc does anything: Terminal and iTerm start bash as a
// login shell, which reads the first of ~/.bash_profile, ~/.bash_login and
// ~/.profile there is, and not ~/.bashrc. Most such files load ~/.bashrc;
// one that does not, or none at all, leaves the block unread, and a name
// defined there alone out of the probe, which reads ~/.bashrc as an
// interactive shell does. Empty when the login file loads ~/.bashrc.
func bashLoginNote(home, name string) string {
	var login string
	for _, file := range []string{".bash_profile", ".bash_login", ".profile"} {
		raw, err := os.ReadFile(filepath.Join(home, file))
		if err != nil {
			continue
		}
		if loadsBashrc(string(raw)) {
			return ""
		}
		login = filepath.Join(home, file)
		break
	}
	const line = "[ -r ~/.bashrc ] && . ~/.bashrc"
	if login == "" {
		return fmt.Sprintf("On macOS, Terminal and iTerm start bash as a login shell, which reads ~/.bash_profile and not ~/.bashrc. You have no ~/.bash_profile, so the block takes effect once you make one with this line: %s", line)
	}
	return fmt.Sprintf("On macOS, Terminal and iTerm start bash as a login shell, which reads %s and not ~/.bashrc, and %s does not load ~/.bashrc. Should it define %s itself, move that into ~/.bashrc and claim again. The block takes effect once you add this line to the end of %s: %s",
		login, filepath.Base(login), name, filepath.Base(login), line)
}

// loadsBashrc reports whether a login file mentions ~/.bashrc outside a
// comment, as the line that loads it does in any of its usual forms.
func loadsBashrc(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#") && strings.Contains(line, ".bashrc") {
			return true
		}
	}
	return false
}

func prepareWindows(env *Env, p *Plan) (*Plan, error) {
	if env.LocalAppData == "" {
		return nil, errors.New("LOCALAPPDATA is not set; cannot find a folder on your PATH for the shim")
	}
	// cmd.exe expands %VAR% in the shim even inside quotes (and !VAR! when
	// delayed expansion is switched on), and a " would end the quoted path
	// early, so with any of them in the path the shim could run something
	// other than firmfact.
	if strings.ContainsAny(env.Self, `%"!`) {
		return nil, fmt.Errorf("firmfact is installed at %s, and a .cmd shim cannot safely run a path with %%, \" or ! in it; move firmfact to a folder without them and claim again", env.Self)
	}
	dir := filepath.Join(env.LocalAppData, "Microsoft", "WindowsApps")
	p.LinkPath = filepath.Join(dir, p.Name+".cmd")
	p.LinkBody = "@rem firmfact claim " + p.Name + "\r\n@\"" + env.Self + "\" %*\r\n"
	if raw, err := os.ReadFile(p.LinkPath); err == nil {
		if string(raw) != p.LinkBody {
			return nil, fmt.Errorf("%s already exists and was not written by firmfact; remove it yourself if you want to replace it", p.LinkPath)
		}
		p.LinkExists = true
	}
	if !onPath(env.Path, dir) {
		p.Notes = append(p.Notes, dir+" is not on your PATH; add it in Settings > System > About > Advanced system settings > Environment Variables.")
	}
	p.Notes = append(p.Notes, "If your PowerShell profile defines "+p.Name+", that alias still wins; remove it from $PROFILE to use the shim.")
	return p, nil
}

// Apply carries out the plan.
func Apply(env *Env, p *Plan) error {
	if !p.LinkExists {
		// The mode any installer gives ~/.local/bin; a directory of programs
		// holds nothing secret.
		if err := os.MkdirAll(filepath.Dir(p.LinkPath), 0o755); err != nil { //nolint:gosec // G301, see above
			return err
		}
		if p.LinkBody != "" {
			// A .cmd shim holds nothing secret, and Windows ignores the mode.
			if err := os.WriteFile(p.LinkPath, []byte(p.LinkBody), 0o644); err != nil { //nolint:gosec // G306, see above
				return err
			}
		} else if target, err := os.Readlink(p.LinkPath); err != nil || !sameFile(target, env.Self) {
			if err := os.Symlink(env.Self, p.LinkPath); err != nil {
				return err
			}
		}
	}
	if p.RCFile == "" || p.RCManual != "" {
		return nil
	}
	existing, err := os.ReadFile(p.RCFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	cleaned := removeBlock(string(existing), p.Name)
	if cleaned != "" && !strings.HasSuffix(cleaned, "\n") {
		cleaned += "\n"
	}
	if len(existing) > 0 {
		backup := p.RCFile + ".before-firmfact-claim-" + time.Now().Format("20060102-150405")
		// G703: the backup sits next to the user's own rc file.
		if err := os.WriteFile(backup, existing, 0o600); err != nil { //nolint:gosec // see above
			return err
		}
		p.Backup = backup
	}
	// The usual mode for a shell's config directory, such as
	// ~/.config/fish/conf.d.
	if err := os.MkdirAll(filepath.Dir(p.RCFile), 0o755); err != nil { //nolint:gosec // G301, see above
		return err
	}
	return writeRC(p.RCFile, cleaned+"\n"+p.RCBlock)
}

// Undo removes the rc block and the link, leaving anything else alone. An
// rc file claim may not change is left as it is, and the error says which
// lines to remove by hand.
func Undo(env *Env, name string) ([]string, error) {
	var done []string
	if env.GOOS == "windows" {
		shim := filepath.Join(env.LocalAppData, "Microsoft", "WindowsApps", name+".cmd")
		if raw, err := os.ReadFile(shim); err == nil && strings.HasPrefix(string(raw), "@rem firmfact claim "+name) {
			if err := os.Remove(shim); err != nil {
				return done, err
			}
			done = append(done, "removed "+shim)
		}
		return done, nil
	}
	link := filepath.Join(env.Home, ".local", "bin", name)
	if target, err := os.Readlink(link); err == nil && sameFile(target, env.Self) {
		if err := os.Remove(link); err != nil {
			return done, err
		}
		done = append(done, "removed "+link)
	}
	var manual []error
	for _, rc := range allRCFiles(env.Home) {
		raw, err := os.ReadFile(rc)
		if err != nil {
			continue
		}
		cleaned := removeBlock(string(raw), name)
		if cleaned == string(raw) {
			continue
		}
		var refused *manualEdit
		if err := writeRC(rc, cleaned); errors.As(err, &refused) {
			manual = append(manual, fmt.Errorf("left %s alone because %s; remove the lines from %q to %q yourself", rc, refused.reason, markerStart(name), markerEnd(name)))
			continue
		} else if err != nil {
			return done, err
		}
		done = append(done, "removed the firmfact block from "+rc)
	}
	return done, errors.Join(manual...)
}

// extractBlock returns the marked block for name, or "".
func extractBlock(content, name string) string {
	start := strings.Index(content, markerStart(name))
	if start < 0 {
		return ""
	}
	end := strings.Index(content[start:], markerEnd(name))
	if end < 0 {
		return content[start:]
	}
	return content[start : start+end+len(markerEnd(name))]
}

func blockIsCurrent(block, shell, name string) bool {
	if shell == "fish" {
		return strings.Contains(block, "set -g __firmfact_"+shellVar(name)+"_block "+blockVersion)
	}
	return strings.Contains(block, "__firmfact_"+shellVar(name)+"_block="+blockVersion)
}

// shellVar is name as the names of the block's variables carry it: a
// command name may have a hyphen, which no shell allows in a variable's.
// validName allows no underscore, so no two names come out the same.
func shellVar(name string) string { return strings.ReplaceAll(name, "-", "_") }

// firstOnPath returns the first executable called name on the PATH, or "".
func firstOnPath(path, name string) string {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

// removeBlock drops the marked block for name (and one blank line before it).
func removeBlock(content, name string) string {
	start, end := markerStart(name), markerEnd(name)
	lines := strings.Split(content, "\n")
	var out []string
	skipping := false
	for _, line := range lines {
		switch {
		case strings.TrimSpace(line) == start:
			skipping = true
			if n := len(out); n > 0 && strings.TrimSpace(out[n-1]) == "" {
				out = out[:n-1]
			}
		case skipping && strings.TrimSpace(line) == end:
			skipping = false
		case !skipping:
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// blockVersion is written into every block, so a later claim can recognise
// and replace an older one. Version 3 finds a bash alias in a way bash 3.2
// understands; version 2 looked in BASH_ALIASES, which bash 3.2 does not
// have, and so left the alias in place there.
const blockVersion = "3"

// takeOver returns the shell lines that make name a small dispatcher:
//
//   - with arguments, it runs the firmfact binary (ff whoami, ff vendors list);
//   - with no arguments it runs the previous definition, so the everyday bare
//     use (Omarchy's ff file picker) and helpers that call it inside a pipe or
//     $(...) (Omarchy's sff and eff) keep working;
//   - when no firmfact binary is on the PATH any more (uninstalled), every
//     call goes to the previous definition, so the block is harmless.
//
// The previous alias is kept as a function that evaluates the saved alias
// text at run time, and functions are declared with the `function` keyword,
// whose name is never alias-expanded, so the block also works when a
// dotfile manager wraps it in a conditional.
// Everything is decided at shell start-up, so an update of the framework that
// defines the old name is picked up.
//
// bash reads the alias text from what `alias NAME` prints, which bash quotes
// for the shell to read back (without the "alias " in POSIX mode), rather
// than from BASH_ALIASES: that came with bash 4, and macOS's /bin/bash is
// still 3.2.
func takeOver(shell, name, keepAs string) []string {
	var tpl string
	switch shell {
	case "fish":
		tpl = `set -g __firmfact_NAME_block VERSION
if functions -q NAME; and not functions -q PREV; and not string match -q '*__firmfact_NAME_dispatch*' -- (functions NAME)
    functions -c NAME PREV
end
abbr -q NAME; and abbr -e NAME
function NAME --description 'firmfact with arguments, your previous NAME without'
    set -l __firmfact_NAME_dispatch 1
    if test (count $argv) -gt 0; and command -q NAME
        command NAME $argv
    else if functions -q PREV
        PREV $argv
    else
        command NAME $argv
    end
end`
	case "zsh":
		tpl = `typeset -g __firmfact_NAME_block=VERSION
if (( ${+aliases[NAME]} )); then
  typeset -g __firmfact_NAME_prev=${aliases[NAME]}
  unalias NAME
  function PREV { eval "$__firmfact_NAME_prev \"\$@\"" }
elif (( ${+functions[NAME]} )) && [[ ${functions[NAME]} != *__firmfact_NAME_dispatch* ]]; then
  autoload +X NAME 2>/dev/null
  functions[PREV]=${functions[NAME]}
  unfunction NAME
fi
if (( ${+functions[PREV]} )); then
  function NAME {
    : __firmfact_NAME_dispatch
    if (( $# )) && (( ${+commands[NAME]} )); then command NAME "$@"
    elif (( ${+functions[PREV]} )); then PREV "$@"
    else command NAME "$@"; fi
  }
fi`
	default: // bash
		tpl = `__firmfact_NAME_block=VERSION
if __firmfact_NAME_prev=$(alias NAME 2>/dev/null); then
  __firmfact_NAME_prev=${__firmfact_NAME_prev#alias }
  eval "__firmfact_NAME_prev=${__firmfact_NAME_prev#NAME=}"
  unalias NAME
  function PREV { eval "$__firmfact_NAME_prev \"\$@\""; }
elif declare -F NAME >/dev/null && [[ $(declare -f NAME) != *__firmfact_NAME_dispatch* ]]; then
  eval "$(declare -f NAME | sed '1s/^NAME /PREV /')"
  unset -f NAME
fi
if declare -F PREV >/dev/null; then
  function NAME {
    : __firmfact_NAME_dispatch
    if (( $# )) && type -P NAME >/dev/null; then command NAME "$@"
    elif declare -F PREV >/dev/null; then PREV "$@"
    else command NAME "$@"; fi
  }
fi`
	}
	// The first pair wins where both match, so the variables get shellVar.
	block := strings.NewReplacer("__firmfact_NAME_", "__firmfact_"+shellVar(name)+"_",
		"NAME", name, "PREV", keepAs, "VERSION", blockVersion).Replace(tpl)
	return strings.Split(block, "\n")
}

// pathLine appends dir to the PATH, so claiming one name never changes which
// program every other command runs. dir comes from $HOME and is quoted the
// way the fish line always was: inside double quotes a $(...), a backtick
// or a " in a home directory's name would run or break at every shell start.
func pathLine(shell, dir string) string {
	if shell == "fish" {
		return "fish_add_path -g -a " + quote(dir)
	}
	return "case \":$PATH:\" in *:" + quote(dir) + ":*) ;; *) export PATH=\"$PATH\":" + quote(dir) + " ;; esac"
}

// addsPath reports whether an earlier claim's block puts a directory on the
// PATH.
func addsPath(block string) bool {
	return strings.Contains(block, "export PATH=") || strings.Contains(block, "fish_add_path")
}

// onlyAddsPath reports whether an earlier claim's block does nothing but
// put a directory on the PATH: past its markers and comments, every line
// is a PATH line, in any of the forms claim has written.
func onlyAddsPath(block string) bool {
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") &&
			!strings.HasPrefix(line, `case ":$PATH:" in `) && !strings.HasPrefix(line, "fish_add_path ") {
			return false
		}
	}
	return true
}

func rcFile(env *Env) (string, error) {
	switch env.Shell {
	case "bash":
		return filepath.Join(env.Home, ".bashrc"), nil
	case "zsh":
		if zd := os.Getenv("ZDOTDIR"); zd != "" {
			return filepath.Join(zd, ".zshrc"), nil
		}
		return filepath.Join(env.Home, ".zshrc"), nil
	case "fish":
		return filepath.Join(fishConfigDir(env.Home), "config.fish"), nil
	}
	return "", fmt.Errorf("don't know how to configure the %q shell; pass --shell bash, zsh or fish", env.Shell)
}

func fishConfigDir(home string) string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "fish")
	}
	return filepath.Join(home, ".config", "fish")
}

func allRCFiles(home string) []string {
	files := []string{
		filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".zshrc"),
		filepath.Join(fishConfigDir(home), "config.fish"),
	}
	if zd := os.Getenv("ZDOTDIR"); zd != "" {
		files = append(files, filepath.Join(zd, ".zshrc"))
	}
	return files
}

// probeTimeout bounds how long the user's shell may take to start and
// answer. It is a variable so tests need not wait that long.
var probeTimeout = 10 * time.Second

// resolveInShell asks the user's own interactive shell, so rc files and
// frameworks are loaded exactly as they are when the user types a command.
// The answer is framed by markers because interactive shells may print
// banners or warnings around it.
func resolveInShell(shell, name string) (kind, detail string, err error) {
	script := resolveScript(shell, name)
	if script == "" {
		return "", "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	// G204: resolveScript only knows bash, zsh and fish, and name has
	// passed validName.
	cmd := exec.CommandContext(ctx, shell, "-ic", script) //nolint:gosec // see above
	// The rc files must not take over the user's terminal.
	detachFromTerminal(cmd)
	// Something an rc file starts in the background can keep the output
	// open after the shell has answered and gone; stop waiting for it soon.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if kind, detail, ok := parseResolve(string(out)); ok {
		return kind, detail, nil
	}
	switch {
	case ctx.Err() != nil:
		return "", "", fmt.Errorf("%s -i did not finish within %s (does your rc file wait for input?)", shell, probeTimeout)
	case cmd.Process == nil:
		return "", "", err // the shell did not start, as when it is not installed
	}
	// The shell ran but never answered, as when an rc file ends by starting
	// another program; nothing is known about name then.
	return "", "", nil
}

// resolveScript prints how the shell sees name, between markers:
// @@kind=alias|function|file|builtin|keyword| and @@detail=<alias value or path>.
func resolveScript(shell, name string) string {
	switch shell {
	case "bash":
		// The alias text as the block reads it; see takeOver.
		return fmt.Sprintf(`t=$(type -t %[1]s); d=; case "$t" in alias) d=$(alias %[1]s); d=${d#alias }; eval "d=${d#%[1]s=}";; file) d=$(command -v %[1]s);; esac; printf '@@kind=%%s\n@@detail=%%s\n' "$t" "$d"`, name)
	case "zsh":
		return fmt.Sprintf(`t=$(whence -w %[1]s); t=${t##*: }; d=; case "$t" in alias) d=${aliases[%[1]s]};; command) t=file; d=$(whence -p %[1]s);; none) t=;; esac; printf '@@kind=%%s\n@@detail=%%s\n' "$t" "$d"`, name)
	case "fish":
		return fmt.Sprintf(`set -l t (type -t %[1]s 2>/dev/null); set -l d; if test "$t" = file; set d (command -v %[1]s); end; printf '@@kind=%%s\n@@detail=%%s\n' "$t" "$d"`, name)
	}
	return ""
}

// parseResolve reads the probe's answer; ok is false when there is none.
func parseResolve(out string) (kind, detail string, ok bool) {
	for _, line := range strings.Split(out, "\n") {
		if v, found := strings.CutPrefix(line, "@@kind="); found {
			kind, ok = strings.TrimSpace(v), true
		} else if v, found := strings.CutPrefix(line, "@@detail="); found {
			detail = strings.TrimSpace(v)
		}
	}
	return kind, detail, ok
}

func onPath(path, dir string) bool {
	for _, p := range filepath.SplitList(path) {
		if sameFile(p, dir) {
			return true
		}
	}
	return false
}

func sameFile(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ca, errA := filepath.EvalSymlinks(a)
	cb, errB := filepath.EvalSymlinks(b)
	if errA == nil && errB == nil {
		return ca == cb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func abbreviate(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 60 {
		return s[:57] + "..."
	}
	return s
}
