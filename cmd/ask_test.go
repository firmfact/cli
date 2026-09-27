package cmd

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/mcp"
)

// chatThreads answers each chat in the thread it asks for, or in a new
// thread, t-1, when it asks for none. A thread the server does not have
// (gone) gets a new one, t-2, as the server's find_or_create_thread does.
// The answer has the shape of execute_chat's data (tool_registry.rb).
func chatThreads(t *testing.T) func(toolCall) string {
	return func(call toolCall) string {
		thread, _ := call.Arguments["thread_id"].(string)
		switch thread {
		case "":
			thread = "t-1"
		case "gone":
			thread = "t-2"
		}
		answer := mustJSON(t, map[string]any{"response": "Answer in " + thread, "thread_id": thread})
		return toolResult(t, answer)
	}
}

// askServer is a signed-in host whose chat answers as chatThreads does.
func askServer(t *testing.T) (*mcpServer, string) {
	t.Helper()
	isolate(t)
	f := &mcpServer{respond: chatThreads(t)}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, chatWithWorkspace)
	return f, srv.URL
}

// ask sends the question as the message and prints the answer as text; the
// thread the answer names is kept, and --continue (or -c) sends it with the
// next question.
func TestAskContinuesTheLastThread(t *testing.T) {
	f, host := askServer(t)

	stdout, stderr, err := run("test", "--host", host, "ask", "Which contracts renew in the next 90 days?")
	if err != nil {
		t.Fatalf("ask: %v\n%s", err, stderr)
	}
	if stdout != "Answer in t-1\n" || stderr != "" {
		t.Errorf("ask: stdout = %q, stderr = %q", stdout, stderr)
	}
	calls := f.calls()
	if len(calls) != 1 || calls[0].Name != chatTool {
		t.Fatalf("calls = %+v", calls)
	}
	first := calls[0].Arguments
	if first["message"] != "Which contracts renew in the next 90 days?" || first["thread_id"] != nil || first["workspace"] != nil {
		t.Errorf("first call's arguments = %v", first)
	}
	if got := loadThread(host, ""); got != "t-1" {
		t.Errorf("kept thread = %q, want t-1", got)
	}

	for i, flag := range []string{"--continue", "-c"} {
		stdout, _, err = run("test", "--host", host, "ask", flag, "Which of those are with LSEG?")
		if err != nil {
			t.Fatalf("ask %s: %v", flag, err)
		}
		if stdout != "Answer in t-1\n" {
			t.Errorf("ask %s: stdout = %q", flag, stdout)
		}
		call := f.calls()[i+1].Arguments
		if call["thread_id"] != "t-1" || call["message"] != "Which of those are with LSEG?" {
			t.Errorf("ask %s: arguments = %v, want the kept thread", flag, call)
		}
	}
}

// --thread picks the thread, and the thread of each answer is the one kept:
// a thread the server cannot find gets a new one without an error, which
// is said, and the next --continue follows the new thread.
func TestAskKeepsTheThreadOfEachAnswer(t *testing.T) {
	f, host := askServer(t)

	_, stderr, err := run("test", "--host", host, "ask", "--thread", "t-9", "Hi")
	if err != nil || stderr != "" {
		t.Fatalf("ask --thread: %v, stderr = %q", err, stderr)
	}
	if got := loadThread(host, ""); got != "t-9" {
		t.Errorf("kept thread = %q, want t-9", got)
	}

	_, stderr, err = run("test", "--host", host, "ask", "--thread", "gone", "Hi")
	if err != nil {
		t.Fatal(err)
	}
	if want := "note: thread gone was not found on " + host + " in your default workspace, so this answer starts a new thread.\n"; stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	if got := loadThread(host, ""); got != "t-2" {
		t.Errorf("kept thread = %q, want the new one, t-2", got)
	}
	if _, _, err := run("test", "--host", host, "ask", "--continue", "Hi"); err != nil {
		t.Fatal(err)
	}
	calls := f.calls()
	if got := calls[len(calls)-1].Arguments["thread_id"]; got != "t-2" {
		t.Errorf("--continue sent %v, want t-2", got)
	}
}

// With -, the question is all of standard input, several lines and all.
// Nothing there is a usage error; a question longer than the server reads
// is sent whole, with a note; and a pipe beyond 1 MiB is refused unread.
func TestAskReadsTheQuestionFromStdin(t *testing.T) {
	f, host := askServer(t)

	stdout, stderr, err := runWithInput("Which contracts\nrenew soon?\n\n", "test", "--host", host, "ask", "-")
	if err != nil || stdout != "Answer in t-1\n" || stderr != "" {
		t.Fatalf("ask -: %v, stdout = %q, stderr = %q", err, stdout, stderr)
	}
	if got := f.calls()[0].Arguments["message"]; got != "Which contracts\nrenew soon?" {
		t.Errorf("message = %q", got)
	}

	for _, input := range []string{"", " \n\t\n"} {
		_, _, err = runWithInput(input, "test", "--host", host, "ask", "-")
		if code, _ := Classify(err); code != ExitUsage || !strings.Contains(err.Error(), "no question") {
			t.Errorf("input %q: err = %v (exit %d), want a usage error", input, err, code)
		}
	}

	long := strings.Repeat("é", maxQuestionChars+5)
	_, stderr, err = runWithInput(long, "test", "--host", host, "ask", "-")
	if err != nil {
		t.Fatal(err)
	}
	if want := "note: the question is 10,005 characters long; the service reads the first 10,000.\n"; stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	calls := f.calls()
	if got, _ := calls[len(calls)-1].Arguments["message"].(string); got != long {
		t.Errorf("the long question was not sent whole (%d bytes of %d)", len(got), len(long))
	}

	_, _, err = runWithInput(strings.Repeat("x", maxStdinQuestion+1), "test", "--host", host, "ask", "-")
	if code, _ := Classify(err); code != ExitUsage || !strings.Contains(err.Error(), "more than 1 MiB") {
		t.Errorf("oversized input: err = %v (exit %d)", err, code)
	}
	if n := len(f.calls()); n != 2 {
		t.Errorf("calls = %d, want only the two questions that could be read", n)
	}
}

// A thread belongs to one host and workspace: the workspace a question was
// asked in is sent, and keeps its own thread, which --continue in another
// workspace does not follow.
func TestAskKeepsAThreadPerWorkspace(t *testing.T) {
	f, host := askServer(t)

	if _, _, err := run("test", "--host", host, "--workspace", "Acme Bank", "ask", "Hi"); err != nil {
		t.Fatal(err)
	}
	if got := f.calls()[0].Arguments["workspace"]; got != "Acme Bank" {
		t.Errorf("workspace sent = %v", got)
	}
	if loadThread(host, "Acme Bank") != "t-1" || loadThread(host, "") != "" {
		t.Errorf("threads kept: Acme Bank %q, default %q", loadThread(host, "Acme Bank"), loadThread(host, ""))
	}

	t.Setenv("FIRMFACT_WORKSPACE", "Acme Bank")
	if _, _, err := run("test", "--host", host, "ask", "--continue", "Hi"); err != nil {
		t.Fatal(err)
	}
	if got := f.calls()[1].Arguments["thread_id"]; got != "t-1" {
		t.Errorf("FIRMFACT_WORKSPACE: --continue sent %v, want t-1", got)
	}
	t.Setenv("FIRMFACT_WORKSPACE", "")

	for _, ws := range []string{"Demo", ""} {
		args := []string{"--host", host, "ask", "--continue", "Hi"}
		want := "there is no earlier question on " + host + " in your default workspace to continue"
		if ws != "" {
			args = append([]string{"--workspace", ws}, args...)
			want = "there is no earlier question on " + host + " in workspace Demo to continue"
		}
		_, _, err := run("test", args...)
		if code, _ := Classify(err); code != ExitNotFound || !strings.Contains(err.Error(), want) {
			t.Errorf("workspace %q: err = %v (exit %d), want %q", ws, err, code, want)
		}
	}
	if n := len(f.calls()); n != 2 {
		t.Errorf("calls = %d, want none for a --continue with nothing to continue", n)
	}
}

// --json prints the answer as every workspace command does, with the
// thread under data, and still keeps the thread.
func TestAskJSON(t *testing.T) {
	_, host := askServer(t)

	stdout, _, err := run("test", "--host", host, "--json", "ask", "Hi")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Data struct {
			Response string `json:"response"`
			ThreadID string `json:"thread_id"`
		} `json:"data"`
	}
	decodeOnly(t, "ask --json", stdout, &got)
	if got.Data.Response != "Answer in t-1" || got.Data.ThreadID != "t-1" {
		t.Errorf("--json = %s", stdout)
	}
	if loadThread(host, "") != "t-1" {
		t.Error("--json did not keep the thread")
	}
}

// A command line ask cannot run is a usage error, and nothing is sent.
func TestAskCommandLineMistakes(t *testing.T) {
	f, host := askServer(t)
	saveThreadOrFail(t, host, "", "t-1")

	cases := []struct {
		args []string
		want string
	}{
		{[]string{"ask"}, `ask needs a question: firmfact ask "...", or firmfact ask - to read it from standard input`},
		{[]string{"ask", "which", "vendors?"}, `the question arrived as 2 separate arguments; put it in quotes: firmfact ask "..."`},
		{[]string{"ask", "  "}, "the question is empty"},
		{[]string{"ask", "--continue", "--thread", "t-1", "Hi"}, "none of the others can be"},
		{[]string{"--format", "csv", "ask", "Hi"}, "--format csv is for workspace commands"},
	}
	for _, c := range cases {
		_, _, err := run("test", append([]string{"--host", host}, c.args...)...)
		if code, _ := Classify(err); code != ExitUsage || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err = %v (exit %d), want a usage error with %q", c.args, err, code, c.want)
		}
	}
	if n := len(f.calls()); n != 0 {
		t.Errorf("calls = %d, want none", n)
	}
}

// Not signed in, ask says so with the status for it.
func TestAskNeedsASignIn(t *testing.T) {
	isolate(t)
	f := &mcpServer{respond: chatThreads(t)}
	srv := f.start(t)
	if err := saveToolCache(srv.URL, []mcp.Tool{chatWithWorkspace}); err != nil {
		t.Fatal(err)
	}
	_, _, err := run("test", "--host", srv.URL, "ask", "Hi")
	if code, _ := Classify(err); code != ExitSignedOut {
		t.Errorf("err = %v (exit %d), want exit %d", err, code, ExitSignedOut)
	}
}

// logout forgets the threads kept for its host, as they belong to the
// sign-in that ended, and leaves those of other hosts.
func TestLogoutForgetsTheThreads(t *testing.T) {
	isolate(t)
	f := &revokeServer{status: http.StatusOK}
	srv := f.start(t)
	storedToken(t, srv.URL, time.Hour)
	saveThreadOrFail(t, srv.URL, "", "t-1")
	saveThreadOrFail(t, srv.URL, "Demo", "t-2")
	saveThreadOrFail(t, "https://other.example", "", "t-3")

	if _, _, err := run("test", "--host", srv.URL, "logout"); err != nil {
		t.Fatal(err)
	}
	if loadThread(srv.URL, "") != "" || loadThread(srv.URL, "Demo") != "" {
		t.Error("logout left a thread of its host")
	}
	if loadThread("https://other.example", "") != "t-3" {
		t.Error("logout removed another host's thread")
	}
}

func saveThreadOrFail(t *testing.T, host, workspace, id string) {
	t.Helper()
	if err := saveThread(host, workspace, id); err != nil {
		t.Fatal(err)
	}
}

// A kept thread is used only for the host and workspace it was kept for,
// named exactly so, whatever file it is found in.
func TestLoadThreadChecksWhereItWasKept(t *testing.T) {
	isolate(t)
	saveThreadOrFail(t, "https://a.example", "Demo", "t-1")
	if got := loadThread("https://a.example", "demo"); got != "" {
		t.Errorf("workspace demo found Demo's thread %q", got)
	}
	path, err := threadPath("https://a.example", "Demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := config.WriteJSON(path, savedThread{Host: "https://b.example", Workspace: "Demo", ThreadID: "t-2"}, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadThread("https://a.example", "Demo"); got != "" {
		t.Errorf("another host's thread in the file was used: %q", got)
	}
	if err := forgetThreads("https://a.example"); err != nil {
		t.Fatal(err)
	}
	if err := forgetThreads("https://a.example"); err != nil {
		t.Errorf("forgetting what is already gone: %v", err)
	}
}
