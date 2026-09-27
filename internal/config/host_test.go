package config

import (
	"errors"
	"strings"
	"testing"
)

func TestParseHostAccepts(t *testing.T) {
	cases := map[string]string{
		"firmfact.com":                  "https://firmfact.com",
		"  https://firmfact.com/  ":     "https://firmfact.com",
		"HTTPS://Staging.FirmFact.COM":  "https://staging.firmfact.com",
		"https://firmfact.com:443":      "https://firmfact.com",
		"https://firmfact.example:8443": "https://firmfact.example:8443",
		"firmfact.example:08443":        "https://firmfact.example:8443",
		"rails_app:3000":                "https://rails_app:3000",
		"https://[2001:db8::1]:8443/":   "https://[2001:db8::1]:8443",
		// This machine: a bare host means plain http, a local dev server.
		"localhost:5000":         "http://localhost:5000",
		"localhost":              "http://localhost",
		"LOCALHOST:5000/":        "http://localhost:5000",
		"http://localhost:5000":  "http://localhost:5000",
		"https://localhost:5000": "https://localhost:5000",
		"127.0.0.1:3000":         "http://127.0.0.1:3000",
		"http://127.1.2.3:80":    "http://127.1.2.3",
		"[::1]:5000":             "http://[::1]:5000",
		"http://[::1]:5000/":     "http://[::1]:5000",
	}
	for in, want := range cases {
		got, err := ParseHost(in)
		if err != nil || got != want {
			t.Errorf("ParseHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestParseHostRejects(t *testing.T) {
	cases := map[string]string{
		"":                                 "no host",
		"   ":                              "no host",
		"firmfact.com@evil.example":        "user name",
		"http://firmfact.com@127.0.0.1:80": "user name",
		"https://user:pass@firmfact.com":   "user name",
		"http://firmfact.example":          "plain http",
		"http://10.0.0.5:3000":             "plain http",
		"http://localhost.evil.example":    "plain http",
		"ftp://x":                          "only https",
		"file:///etc/passwd":               "only https",
		"javascript:alert(1)":              "not a valid host",
		"https:firmfact.com":               "not a valid host",
		"https://firmfact.com/api":         "without a path",
		"//firmfact.com":                   "without a path",
		"https://firmfact.com?next=/x":     "without a path",
		"https://firmfact.com/?":           "without a path",
		"https://firmfact.com#top":         "without a path",
		"https://":                         "no host name",
		"https://:5000":                    "no host name",
		"https://firmfact.com:0":           "port",
		"https://firmfact.com:65536":       "port",
		"firm fact.com":                    "not a valid host",
		"https://fïrmfact.com":             "not allowed",
		"https://firm!fact.com":            "not allowed",
		"https://.firmfact.com":            "not a valid host name",
		"https://2130706433":               "not a valid IP",
		"http://0127.0.0.1:5000":           "not a valid IP",
		"::1":                              "not allowed",
		"https://[fe80::1%25eth0]":         "zone",
	}
	for in, want := range cases {
		got, err := ParseHost(in)
		if err == nil {
			t.Errorf("ParseHost(%q) = %q, want an error", in, got)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ParseHost(%q) error %q, want it to mention %q", in, err, want)
		}
	}
}

// --insecure-http lifts the http rule and nothing else.
func TestParseHostAllowHTTP(t *testing.T) {
	got, err := ParseHostAllowHTTP("http://Firmfact.Example:80/")
	if err != nil || got != "http://firmfact.example" {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := ParseHost("http://firmfact.example"); !errors.Is(err, ErrPlainHTTP) {
		t.Errorf("without the flag: %v, want ErrPlainHTTP", err)
	}
	for _, in := range []string{"http://firmfact.com@127.0.0.1:5000", "ftp://x", "http://firmfact.example/api"} {
		if got, err := ParseHostAllowHTTP(in); err == nil {
			t.Errorf("ParseHostAllowHTTP(%q) = %q, want an error", in, got)
		}
	}
}

func TestNeedsInsecureHTTP(t *testing.T) {
	cases := map[string]bool{
		"http://firmfact.example": true,
		"http://10.0.0.5:3000":    true,
		"https://firmfact.com":    false,
		"http://localhost:5000":   false,
		"http://127.0.0.1:5000":   false,
		"http://[::1]:5000":       false,
	}
	for host, want := range cases {
		if got := NeedsInsecureHTTP(host); got != want {
			t.Errorf("NeedsInsecureHTTP(%q) = %v, want %v", host, got, want)
		}
	}
}

// A stored host is checked like --host; an empty or invalid one is an
// error, not the default host.
func TestProfileParsedHost(t *testing.T) {
	cases := []struct {
		p       Profile
		allow   bool
		want    string
		wantErr bool
	}{
		{p: Profile{Host: "https://firmfact.com/"}, want: "https://firmfact.com"},
		{p: Profile{Host: ""}, wantErr: true},
		{p: Profile{Host: "http://firmfact.example"}, wantErr: true},
		{p: Profile{Host: "http://firmfact.example", InsecureHTTP: true}, want: "http://firmfact.example"},
		{p: Profile{Host: "http://firmfact.example"}, allow: true, want: "http://firmfact.example"},
		{p: Profile{Host: "https://firmfact.com@evil.example"}, allow: true, wantErr: true},
	}
	for _, c := range cases {
		got, err := c.p.ParsedHost(c.allow)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("%+v (allow %v): got %q, %v", c.p, c.allow, got, err)
		}
	}
}
