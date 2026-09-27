package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// The host decides where the CLI sends its bearer token, so it is parsed
// strictly rather than patched up. A loose parse once let
// `--host http://firmfact.com@127.0.0.1:PORT` send the token in cleartext to
// a host that only looked like firmfact.

// ErrPlainHTTP is the refusal of plain http to a host other than this
// machine. --insecure-http (ParseHostAllowHTTP) lifts it.
var ErrPlainHTTP = errors.New("plain http is only allowed for localhost, 127.0.0.0/8 and ::1; use https, or add --insecure-http to send your token unencrypted")

// ParseHost validates a firmfact host and returns it as scheme://host[:port],
// lower-cased and without a default port, so one host always has one
// spelling (it keys the stored tokens and caches).
//
// It accepts https to any host and plain http to this machine only. A host
// without a scheme gets https, except this machine, which gets http:
// "localhost:5000" is a local development server. User names, paths other
// than "/", queries and fragments are refused rather than dropped.
func ParseHost(s string) (string, error) { return parseHost(s, false) }

// ParseHostAllowHTTP is ParseHost for --insecure-http: plain http is
// accepted for any host.
func ParseHostAllowHTTP(s string) (string, error) { return parseHost(s, true) }

// NeedsInsecureHTTP reports whether a parsed host is plain http to another
// machine and so only valid with --insecure-http.
func NeedsInsecureHTTP(host string) bool {
	u, err := url.Parse(host)
	return err == nil && u.Scheme == "http" && !isThisMachine(u.Hostname())
}

func parseHost(s string, allowHTTP bool) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", errors.New("no host given")
	case strings.Contains(s, "@"):
		return "", fmt.Errorf("%q: a host cannot contain a user name or password (the part before @)", s)
	case strings.ContainsAny(s, "?#"):
		return "", fmt.Errorf("%q: give just the host, without a path, query or fragment", s)
	}
	// A bare host has no scheme; "//" makes url.Parse read it as a host
	// rather than as a scheme ("localhost:5000") or a path ("firmfact.com").
	bare := !strings.Contains(s, "://")
	raw := s
	if bare {
		raw = "//" + s
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%q is not a valid host", s)
	}
	switch {
	case !bare && u.Scheme != "https" && u.Scheme != "http":
		return "", fmt.Errorf("%q: only https (or http to this machine) is supported", s)
	case u.User != nil:
		return "", fmt.Errorf("%q: a host cannot contain a user name or password (the part before @)", s)
	case u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "":
		return "", fmt.Errorf("%q: give just the host, without a path, query or fragment", s)
	}

	name := strings.ToLower(u.Hostname())
	if err := checkHostname(name); err != nil {
		return "", fmt.Errorf("%q: %w", s, err)
	}
	scheme := u.Scheme
	if bare {
		scheme = "https"
		if isThisMachine(name) {
			scheme = "http"
		}
	}
	if scheme == "http" && !allowHTTP && !isThisMachine(name) {
		return "", fmt.Errorf("%q: %w", s, ErrPlainHTTP)
	}

	host := name
	if strings.Contains(name, ":") {
		host = "[" + name + "]"
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("%q: the port must be a number from 1 to 65535", s)
		}
		if !(scheme == "https" && n == 443) && !(scheme == "http" && n == 80) {
			host += ":" + strconv.Itoa(n)
		}
	}
	return scheme + "://" + host, nil
}

// checkHostname allows DNS names (letters, digits, hyphens, dots and the
// underscores container networks use) and IP addresses. url.Parse lets
// through characters such as ! $ ; and , which no firmfact host has.
func checkHostname(name string) error {
	if name == "" {
		return errors.New("no host name")
	}
	if ip := net.ParseIP(name); ip != nil {
		return nil
	}
	// Digits and dots that are not an IP address (0127.0.0.1, 2130706433)
	// mean different machines to different resolvers.
	if strings.Trim(name, "0123456789.") == "" {
		return errors.New("not a valid IP address")
	}
	if strings.Contains(name, "%") {
		return errors.New("IPv6 zone identifiers are not supported")
	}
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "-") || strings.Contains(name, "..") {
		return errors.New("not a valid host name")
	}
	for _, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_'
		if !ok {
			return fmt.Errorf("%q is not allowed in a host name", r)
		}
	}
	return nil
}

// isThisMachine reports whether a host name is the loopback interface:
// localhost, 127.0.0.0/8 or ::1. Only there may the token travel over plain
// http without --insecure-http.
func isThisMachine(name string) bool {
	if name == "localhost" {
		return true
	}
	ip := net.ParseIP(name)
	return ip != nil && ip.IsLoopback()
}
