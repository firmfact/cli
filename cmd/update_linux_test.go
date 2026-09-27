package cmd

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/update"
)

// On a terminal, --version to a release older than this one asks first. No
// changes nothing and downloads nothing; yes goes ahead, to the same
// checks as ever (here the release has no signature).
func TestUpdateAsksBeforeGoingBack(t *testing.T) {
	isolate(t)
	directDownload(t)
	t.Setenv("TERM", "dumb")
	t.Setenv("NO_COLOR", "1")
	host := meServer(t).URL
	g := &github{latest: "v0.2.1", files: map[string]bool{"0.1.0": false}}
	g.serve(t)
	unchanged := selfUnchanged(t)

	for _, c := range []struct {
		answer    string
		downloads int
		shown     string
		err       string
	}{
		{"n", 0, "Nothing changed.", ""},
		{"y", 2, "Installing firmfact 0.1.0 in place of 0.2.0...", "release v0.1.0 has no checksums.txt.sig, so it cannot be verified; not installing"},
	} {
		ty, tty := newTypist(t)
		done := make(chan error, 1)
		go func() {
			done <- NewRootCommand(Build{Version: "0.2.0"}, []string{"--host", host, "update", "--version", "0.1.0"},
				IOStreams{In: tty, Out: tty, Err: &bytes.Buffer{}}).Execute()
		}()
		ty.answer("0.1.0 is older than the 0.2.0 you have. Install it all the same? (y/N): ", c.answer)
		var err error
		select {
		case err = <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("update did not finish")
		}
		tty.Close()
		<-ty.closed
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != c.err {
			t.Errorf("answered %s: error %q, want %q", c.answer, got, c.err)
		}
		if shown := ty.shown.String(); !strings.Contains(shown, c.shown) {
			t.Errorf("answered %s, the terminal showed:\n%s", c.answer, shown)
		}
		if n := g.downloads(); n != c.downloads {
			t.Errorf("answered %s: %d downloads, want %d", c.answer, n, c.downloads)
		}
	}
	unchanged()
}

// Someone on a pre-release hears of a newer one from the daily check, which
// reads GitHub's feed of releases for them and not the latest-release page,
// and is told how to take it. Someone on a release, with the same check
// behind them, is not offered it.
func TestTheDailyCheckOffersNewerPreReleases(t *testing.T) {
	isolate(t)
	t.Setenv("FIRMFACT_NO_UPDATE_CHECK", "")
	t.Setenv("CI", "")
	t.Setenv("TERM", "dumb")
	t.Setenv("NO_COLOR", "1")
	cacheDir, err := config.CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	g := &github{feed: []string{"v0.2.0-rc.2", "untagged-4b8e2f0c7d1a", "v0.2.0-rc.1"}}
	g.serve(t)
	// whoami answers once the check beside it has read the feed.
	host := versionServer(t, func(r *http.Request) {
		untilChecked(cacheDir, r, func(s update.Status) bool { return s.Newest != "" })
	})

	if _, err := runOnTerminal(t, "0.2.0-rc.1", "--host", host, "whoami"); err != nil {
		t.Fatal(err)
	}
	if g.asked("/firmfact/cli/releases.atom") != 1 || g.asked("/firmfact/cli/releases/latest") != 0 {
		t.Errorf("the check asked for the feed %d and the latest release %d time(s); want the feed once",
			g.asked("/firmfact/cli/releases.atom"), g.asked("/firmfact/cli/releases/latest"))
	}

	notice := func(version string) string {
		t.Helper()
		tty, shown := terminal(t, 80)
		var errOut bytes.Buffer
		err := NewRootCommand(Build{Version: version}, []string{"--host", host, "whoami"}, IOStreams{In: tty, Out: tty, Err: &errOut}).Execute()
		shown()
		if err != nil {
			t.Fatalf("%s: %v", version, err)
		}
		return errOut.String()
	}
	want := "firmfact 0.2.0-rc.2 is available (you have 0.2.0-rc.1). Upgrade with: " + upgradeTo(update.InstallMethod(), "firmfact", "0.2.0-rc.2") + "\n"
	if got := notice("0.2.0-rc.1"); got != want {
		t.Errorf("on 0.2.0-rc.1, stderr %q, want %q", got, want)
	}
	if got := notice("0.1.5"); got != "" {
		t.Errorf("on 0.1.5, stderr %q; a release is never offered a pre-release", got)
	}
	if n := g.asked("/firmfact/cli/releases.atom") + g.asked("/firmfact/cli/releases/latest"); n != 1 {
		t.Errorf("GitHub was asked %d times, want once today", n)
	}
}
