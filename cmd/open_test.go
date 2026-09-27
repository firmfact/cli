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
