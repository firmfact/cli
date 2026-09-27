package config

import (
	"errors"
	"fmt"
	"sort"
)

// A profile exists when the file holds it. The default profile and the
// current one exist even before anything was saved to them: they are what
// a fresh install uses, with the default host. Any other name has to be
// created (`config use-profile --create`), so that a typo cannot quietly
// make a profile that points at production.

// Lookup returns the named profile, and whether it exists. A profile that
// exists without being in the file comes back as a new one would be,
// without being added; change a profile through Profile.
func (c *Config) Lookup(name string) (*Profile, bool) {
	if p, ok := c.Profiles[name]; ok {
		return p, true
	}
	if name == DefaultProfile || name == c.CurrentProfile {
		return &Profile{Host: DefaultHost}, true
	}
	return nil, false
}

// Peek returns the named profile for reading: one that does not exist
// reads as a new one would, and is not added.
func (c *Config) Peek(name string) *Profile {
	if p, ok := c.Lookup(name); ok {
		return p
	}
	return &Profile{Host: DefaultHost}
}

// Names lists the profiles that exist, sorted.
func (c *Config) Names() []string {
	seen := map[string]bool{DefaultProfile: true, c.CurrentProfile: true}
	for name := range c.Profiles {
		seen[name] = true
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// Rename gives the profile from the name to, keeping its host and
// workspace; the current profile stays current under its new name. The
// caller has checked that from exists and to does not. Sign-ins are
// stored by host, so they are not affected.
func (c *Config) Rename(from, to string) {
	p := c.Profile(from)
	delete(c.Profiles, from)
	c.Profiles[to] = p
	if c.CurrentProfile == from {
		c.CurrentProfile = to
	}
}

// Delete removes the named profile.
func (c *Config) Delete(name string) { delete(c.Profiles, name) }

// maxProfileName bounds a profile name; it is typed on command lines and
// shown in lists.
const maxProfileName = 64

// CheckProfileName vets the name for a new profile: letters, digits, '.',
// '_' and '-', starting with a letter or digit. Such a name needs no
// quoting in a shell or an environment variable. Names already in a file
// are used as they are.
func CheckProfileName(name string) error {
	switch {
	case name == "":
		return errors.New("a profile needs a name")
	case len(name) > maxProfileName:
		return fmt.Errorf("a profile name has at most %d characters", maxProfileName)
	}
	for i, r := range name {
		letterOrDigit := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !letterOrDigit && (i == 0 || r != '.' && r != '_' && r != '-') {
			return fmt.Errorf("%q: a profile name has letters, digits, '.', '_' and '-', and starts with a letter or digit", name)
		}
	}
	return nil
}
