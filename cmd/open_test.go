package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/update"
)

// open hands the root of the host in use to the browser: the profile's,
// or the one --host names; --json says which it opened.
func TestOpen(t *testing.T) {
	isolate(t)
	var opened []string
	useBrowser(t, func(u string) error {
		opened = append(opened, u)
		return nil
	})

	stdout, stderr, err := run("test", "open")
	if err != nil || stdout != "Opened https://firmfact.com/ in your browser.\n" || stderr != "" {
		t.Errorf("open = %q, stderr %q (%v)", stdout, stderr, err)
	}
	stdout, _, err = run("test", "--host", "localhost:5000", "--json", "open")
	if err != nil {
		t.Fatal(err)
	}
	var got openResult
	decodeOnly(t, "open", stdout, &got)
	if got.URL != "http://localhost:5000/" {
		t.Errorf("open --json = %+v", got)
	}
	t.Setenv("FIRMFACT_HOST", "firmfact.example")
	if _, _, err := run("test", "open"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"https://firmfact.com/", "http://localhost:5000/", "https://firmfact.example/"}; !slices.Equal(opened, want) {
		t.Errorf("opened %q, want %q", opened, want)
	}

	if code, msg := exitStatusOf(t.Context(), "test", "open", "vendors"); code != ExitUsage {
		t.Errorf("open vendors: exit %d (%s), want %d", code, msg, ExitUsage)
	}
}

// Without a browser, the error gives the address to open by hand.
func TestOpenWithoutABrowser(t *testing.T) {
	isolate(t)
	useBrowser(t, func(string) error { return errors.New(`exec: "xdg-open": executable file not found in $PATH`) })
	_, _, err := run("test", "--host", "https://firmfact.example", "open")
	want := `could not open your browser (exec: "xdg-open": executable file not found in $PATH); open https://firmfact.example/ yourself`
	if code, _ := Classify(err); code != ExitFailed || err.Error() != want {
		t.Errorf("err = %v (exit %d), want %q", err, code, want)
	}
}

// A version below the host's minimum still opens the web app, which does
// not care which CLI asked.
func TestOpenBelowTheMinimumVersion(t *testing.T) {
	isolate(t)
	t.Setenv("FIRMFACT_NO_UPDATE_CHECK", "")
	prevVersion := api.Version
	t.Cleanup(func() { api.Version = prevVersion })
	useBrowser(t, func(string) error { return nil })
	cacheDir, err := config.CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(update.Status{CheckedAt: time.Now(), Host: config.DefaultHost, Minimum: "9.9.9"})
	if err := os.WriteFile(filepath.Join(cacheDir, "version-check.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := run("0.1.0", "whoami"); err == nil {
		t.Fatal("whoami ran below the minimum; the test does not set up what it thinks")
	}
	if _, _, err := run("0.1.0", "open"); err != nil {
		t.Errorf("open on 0.1.0 below 9.9.9: %v", err)
	}
}

// open takes a page of the host in use, by its link or its path, such as a
// document's review link, and opens that page of it; --json says which.
func TestOpenAPage(t *testing.T) {
	isolate(t)
	var opened []string
	useBrowser(t, func(u string) error {
		opened = append(opened, u)
		return nil
	})
	const review = "/accounts/1977ef5a-480b-4ea7-94ce-ce9fe4f308fd/documents/423a2262-85dd-4cf1-9b51-60c7bbf2ff7d"
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"open", "https://firmfact.com" + review}, "https://firmfact.com" + review},
		{[]string{"open", review}, "https://firmfact.com" + review},
		// A host is spelt one way whatever the link's spelling.
		{[]string{"open", "HTTPS://FirmFact.com:443/documents?tab=review#line-3"}, "https://firmfact.com/documents?tab=review#line-3"},
		{[]string{"open", "https://firmfact.com"}, "https://firmfact.com/"},
		{[]string{"open", "/a b"}, "https://firmfact.com/a%20b"},
		{[]string{"--host", "localhost:5000", "open", "http://localhost:5000" + review}, "http://localhost:5000" + review},
	}
	for _, c := range cases {
		stdout, stderr, err := run("test", c.args...)
		if err != nil || stdout != "Opened "+c.want+" in your browser.\n" || stderr != "" {
			t.Errorf("%q = %q, stderr %q (%v); want %s", c.args, stdout, stderr, err, c.want)
		}
	}
	stdout, _, err := run("test", "--json", "open", review)
	if err != nil {
		t.Fatal(err)
	}
	var got openResult
	decodeOnly(t, "open --json", stdout, &got)
	if got.URL != "https://firmfact.com"+review {
		t.Errorf("open --json = %+v", got)
	}
	if len(opened) != len(cases)+1 {
		t.Errorf("opened %q", opened)
	}
}

// Nothing but a page of the host in use reaches the browser: not a page of
// another host, whose --host the error names, nor a file, another scheme,
// or a word the opener would read as an option.
func TestOpenRefusesOtherPages(t *testing.T) {
	isolate(t)
	useBrowser(t, func(u string) error {
		t.Errorf("opened %s", u)
		return nil
	})
	cases := map[string]string{
		"https://staging.firmfact.com/accounts/a":  "https://staging.firmfact.com/accounts/a is a page of https://staging.firmfact.com, not of https://firmfact.com, the host you use; add --host https://staging.firmfact.com to open it",
		"http://firmfact.com/accounts/a":           "http://firmfact.com/accounts/a is a page of http://firmfact.com, not of https://firmfact.com, the host you use; add --host http://firmfact.com to open it",
		"https://jan@firmfact.com/accounts/a":      "https://jan@firmfact.com/accounts/a is not a link to a page of https://firmfact.com",
		"https://firmfact.com:99999/":              "https://firmfact.com:99999/ is not a link to a page of https://firmfact.com",
		"https://bad_host!/":                       "https://bad_host!/ is not a link to a page of https://firmfact.com",
		"https://firmfact.com/%zz":                 "https://firmfact.com/%zz is not a link or a path",
		"https:///accounts":                        "https:///accounts is not a link to a page of https://firmfact.com",
		"//evil.example/accounts":                  "open takes a page of https://firmfact.com: its link, or its path such as /accounts/...; //evil.example/accounts is neither",
		"file:///etc/passwd":                       "open takes a page of https://firmfact.com: its link, or its path such as /accounts/...; file:///etc/passwd is neither",
		"accounts/a":                               "open takes a page of https://firmfact.com: its link, or its path such as /accounts/...; accounts/a is neither",
		"-a":                                       "",
		"javascript:alert(1)":                      "open takes a page of https://firmfact.com: its link, or its path such as /accounts/...; javascript:alert(1) is neither",
		"https://firmfact.com/\x1b[2Jaccounts":     "",
		"https://firmfact.com.evil.example/review": "https://firmfact.com.evil.example/review is a page of https://firmfact.com.evil.example, not of https://firmfact.com, the host you use; add --host https://firmfact.com.evil.example to open it",
	}
	for page, want := range cases {
		_, _, err := run("test", "open", "--", page)
		if code, _ := Classify(err); code != ExitUsage {
			t.Errorf("open %q: exit %d (%v), want %d", page, code, err, ExitUsage)
			continue
		}
		if want != "" && err.Error() != want {
			t.Errorf("open %q: %v\nwant %s", page, err, want)
		}
	}
	if code, msg := exitStatusOf(t.Context(), "test", "open", "/a", "/b"); code != ExitUsage {
		t.Errorf("open with two pages: exit %d (%s), want %d", code, msg, ExitUsage)
	}
}
