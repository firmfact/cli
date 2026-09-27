package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/firmfact/cli/cmd"
	"github.com/firmfact/cli/internal/config"
)

// ptyRun is the program running in a child process on a pseudo-terminal,
// the way a user runs it: the terminal is its stdin, stdout and stderr, and
// its controlling terminal, so a ^C typed on it sends SIGINT.
type ptyRun struct {
	t      *testing.T
	master *os.File
	slave  *os.File // kept open here to read the terminal's settings
	child  *exec.Cmd
	mu     sync.Mutex
	out    []byte
}

func startPTY(t *testing.T, args ...string) *ptyRun {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminals here: %v", err)
	}
	t.Cleanup(func() { master.Close() })
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatalf("unlock pty: %v", err)
	}
	n, err := unix.IoctlGetUint32(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatalf("pty number: %v", err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open pty: %v", err)
	}
	t.Cleanup(func() { slave.Close() })

	raw, _ := json.Marshal(args)
	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(), childArgsEnv+"="+string(raw), "NO_COLOR=1", "TERM=xterm")
	child.Stdin, child.Stdout, child.Stderr = slave, slave, slave
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill() })

	p := &ptyRun{t: t, master: master, slave: slave, child: child}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			p.mu.Lock()
			p.out = append(p.out, buf[:n]...)
			p.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return p
}

func (p *ptyRun) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return string(p.out)
}

func (p *ptyRun) echo() bool {
	p.t.Helper()
	tios, err := unix.IoctlGetTermios(int(p.slave.Fd()), unix.TCGETS)
	if err != nil {
		p.t.Fatalf("read terminal settings: %v", err)
	}
	return tios.Lflag&unix.ECHO != 0
}

func (p *ptyRun) waitFor(what string, cond func() bool) {
	p.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			p.t.Fatalf("gave up waiting for %s; output so far %q", what, p.output())
		}
	}
}

// interrupt types ^C and returns the child's exit status.
func (p *ptyRun) interrupt() int {
	p.t.Helper()
	if _, err := io.WriteString(p.master, "\x03"); err != nil {
		p.t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- p.child.Wait() }()
	select {
	case err := <-exited:
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		if err != nil {
			p.t.Fatal(err)
		}
		return 0
	case <-time.After(10 * time.Second):
		p.t.Fatalf("still running 10s after ^C; output %q", p.output())
		return -1
	}
}

const signupOptions = `{"data":{"organization_types":[{"value":"financial_services","label":"Financial services"}],"default_currency":"EUR","terms":[]}}`

func signupArgs(host string) []string {
	return []string{"--host", host, "signup", "--email", "jan@yourfirm.example", "--name", "Jan", "--org", "Bank BV",
		"--type", "financial_services", "--currency", "EUR", "--language", "en", "--accept-terms"}
}

// Ctrl-C at signup's password prompt, where echo is off, gives the terminal
// back with echo on and exits 130. It used to leave the shell not echoing
// what the user typed.
func TestInterruptAtPasswordPromptRestoresEcho(t *testing.T) {
	isolate(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/signup/options" {
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
		io.WriteString(w, signupOptions)
	}))
	defer srv.Close()

	p := startPTY(t, signupArgs(srv.URL)...)
	if !p.echo() {
		t.Fatal("a new terminal should echo")
	}
	p.waitFor("the password prompt with echo off", func() bool {
		return strings.Contains(p.output(), "Password (") && !p.echo()
	})
	if code := p.interrupt(); code != 130 {
		t.Errorf("exit status %d, want 130; output %q", code, p.output())
	}
	if !p.echo() {
		t.Error("echo is still off after ^C")
	}
	// The prompt's line is ended once, and the interrupt is no error.
	p.waitFor("the prompt's line to end", func() bool { return strings.HasSuffix(p.output(), "digit): \r\n") })
	if strings.Contains(p.output(), "error:") {
		t.Errorf("output = %q", p.output())
	}
}

// Ctrl-C at an ordinary prompt ends it, although the read of the terminal
// cannot be interrupted; with the signal caught it would otherwise wait for
// Enter. The terminal echoed ^C after the prompt, and that line is ended.
func TestInterruptAtAPrompt(t *testing.T) {
	isolate(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, signupOptions)
	}))
	defer srv.Close()

	p := startPTY(t, "--host", srv.URL, "signup", "--email", "jan@yourfirm.example")
	p.waitFor("the name prompt", func() bool { return strings.HasSuffix(p.output(), "Your name: ") })
	if code := p.interrupt(); code != 130 {
		t.Errorf("exit status %d, want 130; output %q", code, p.output())
	}
	p.waitFor("the prompt's line to end", func() bool { return strings.HasSuffix(p.output(), "Your name: ^C\r\n") })
	if !p.echo() {
		t.Error("echo is off after ^C")
	}
}

// Ctrl-C while a request hangs abandons it and exits 130, and the line the
// terminal echoed ^C on is ended, so the shell prompt starts on its own.
func TestInterruptDuringARequest(t *testing.T) {
	isolate(t)
	asked := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)

	p := startPTY(t, signupArgs(srv.URL)...)
	select {
	case <-asked:
	case <-time.After(10 * time.Second):
		t.Fatalf("no request arrived; output %q", p.output())
	}
	if code := p.interrupt(); code != 130 {
		t.Errorf("exit status %d, want 130; output %q", code, p.output())
	}
	p.waitFor("the ^C line to end", func() bool { return strings.HasSuffix(p.output(), "^C\r\n") })
	if strings.Contains(p.output(), "error:") {
		t.Errorf("output = %q", p.output())
	}
}

// --password-stdin in a terminal would read the password with echo on, so
// it is refused, pointing at the prompt, before anything is sent.
func TestPasswordStdinRefusesATerminal(t *testing.T) {
	isolate(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
	}))
	defer srv.Close()

	p := startPTY(t, append(signupArgs(srv.URL), "--password-stdin")...)
	exited := make(chan error, 1)
	go func() { exited <- p.child.Wait() }()
	select {
	case err := <-exited:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != cmd.ExitUsage {
			t.Errorf("exit %v, want status %d; output %q", err, cmd.ExitUsage, p.output())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("still running; output %q", p.output())
	}
	p.waitFor("the refusal", func() bool {
		return strings.Contains(p.output(), "--password-stdin reads the password from a pipe, not a terminal")
	})
}

// On a terminal, a workspace command whose tool list is more than a day old
// fetches the list again beside its own call, and the next command has the
// fresh one.
func TestStaleToolListIsRefreshedOnATerminal(t *testing.T) {
	isolate(t)
	var lists atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("mcp-session-id", "sess-1")
		result := `{}`
		switch req.Method {
		case "tools/list":
			const readsOnly = `"annotations":{"readOnlyHint":true,"destructiveHint":false}`
			tools := `{"name":"list_vendors","title":"List vendors","inputSchema":{"type":"object"},` + readsOnly + `}`
			if lists.Add(1) > 1 {
				tools += `,{"name":"list_contracts","title":"List contracts","inputSchema":{"type":"object"},` + readsOnly + `}`
			}
			result = `{"tools":[` + tools + `]}`
		case "tools/call":
			result = `{"isError":false,"content":[{"type":"text","text":"[{\"name\":\"Acme\"}]"}]}`
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":%s}`, req.ID, result)
	}))
	defer srv.Close()
	if err := config.SaveToken(srv.URL, &config.Token{AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := cmd.NewRootCommand(cmd.Build{Version: "test"}, []string{"--host", srv.URL, "tools", "refresh"}, cmd.IOStreams{}).Execute(); err != nil {
		t.Fatal(err)
	}
	cached := toolCacheFile(t)
	var tc map[string]any
	raw, err := os.ReadFile(cached)
	if err == nil {
		err = json.Unmarshal(raw, &tc)
	}
	if err != nil {
		t.Fatal(err)
	}
	tc["fetched_at"] = time.Now().Add(-25 * time.Hour)
	raw, _ = json.Marshal(tc)
	if err := os.WriteFile(cached, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	p := startPTY(t, "--host", srv.URL, "vendors", "list")
	if err := p.child.Wait(); err != nil {
		t.Fatalf("vendors list: %v; output %q", err, p.output())
	}
	p.waitFor("the vendors", func() bool { return strings.Contains(p.output(), "Acme") })
	if n := lists.Load(); n != 2 {
		t.Errorf("tools/list requests = %d, want 2", n)
	}
	raw, err = os.ReadFile(cached)
	if err != nil || !strings.Contains(string(raw), "list_contracts") {
		t.Fatalf("cache after the command = %s, %v", raw, err)
	}
	var fresh struct {
		FetchedAt time.Time `json:"fetched_at"`
	}
	if err := json.Unmarshal(raw, &fresh); err != nil || time.Since(fresh.FetchedAt) > time.Minute {
		t.Errorf("fetched_at = %v, %v; want just now", fresh.FetchedAt, err)
	}
}

// On a terminal a tool that may delete or overwrite data asks first: no
// sends nothing and says so, yes calls the tool.
func TestDestructiveToolAsksOnATerminal(t *testing.T) {
	isolate(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("mcp-session-id", "sess-1")
		result := `{}`
		switch req.Method {
		case "tools/list":
			result = `{"tools":[{"name":"delete_vendor","title":"Delete a vendor",` +
				`"inputSchema":{"type":"object","properties":{"id":{"type":"string"}}},` +
				`"annotations":{"readOnlyHint":false,"destructiveHint":true}}]}`
		case "tools/call":
			calls.Add(1)
			result = `{"isError":false,"content":[{"type":"text","text":"{\"deleted\":\"v1\"}"}]}`
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":%s}`, req.ID, result)
	}))
	defer srv.Close()
	if err := config.SaveToken(srv.URL, &config.Token{AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := cmd.NewRootCommand(cmd.Build{Version: "test"}, []string{"--host", srv.URL, "tools", "refresh"}, cmd.IOStreams{}).Execute(); err != nil {
		t.Fatal(err)
	}

	for _, answer := range []string{"n", "y"} {
		p := startPTY(t, "--host", srv.URL, "--workspace", "Acme Bank", "delete-vendor", "--id", "v1")
		question := "`firmfact delete-vendor` may delete or overwrite data in workspace \"Acme Bank\". Go ahead? (y/N): "
		p.waitFor("the question", func() bool { return strings.Contains(p.output(), question) })
		if n := calls.Load(); n != 0 {
			t.Fatalf("calls = %d before the answer", n)
		}
		if _, err := io.WriteString(p.master, answer+"\r"); err != nil {
			t.Fatal(err)
		}
		if err := p.child.Wait(); err != nil {
			t.Fatalf("answer %s: %v; output %q", answer, err, p.output())
		}
		if answer == "n" {
			p.waitFor("the refusal", func() bool { return strings.Contains(p.output(), "Nothing changed.") })
			if n := calls.Load(); n != 0 {
				t.Errorf("calls = %d after no, want none", n)
			}
			continue
		}
		p.waitFor("the answer", func() bool { return strings.Contains(p.output(), "deleted: v1") })
		if n := calls.Load(); n != 1 {
			t.Errorf("calls = %d after yes, want 1", n)
		}
	}
}

// toolCacheFile is the one tool list cached so far.
func toolCacheFile(t *testing.T) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(os.Getenv("FIRMFACT_CACHE_DIR"), "tools-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("tool caches = %v, %v; want one", files, err)
	}
	return files[0]
}
