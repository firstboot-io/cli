package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Profiles, and the decision that shapes the whole CLI.
//
// A PROFILE IS A TOKEN IS AN ORGANIZATION. An API token is pinned to one
// organization for its whole life, so there is deliberately no `--org` flag and
// there must never be one: an organization is not something a command chooses,
// it is a property of the credential the command authenticates with. Two
// organizations means two profiles.
//
// That is the reason this is a breaking decision rather than a preference. A
// CLI that grew an `--org` flag later would have to answer what happens when it
// disagrees with the token, and the only honest answers are "ignore the flag" or
// "fail" -- both worse than not having it.
//
// The config file holds everything EXCEPT the token. See secret.go for where
// that goes and why.

// Config is the whole file.
type Config struct {
	// DefaultProfile is used when no --profile is given and FIRSTBOOT_TOKEN is
	// not set.
	DefaultProfile string `toml:"default_profile"`
	// Profiles is keyed by name.
	Profiles map[string]Profile `toml:"profiles"`
}

// Profile is one account, which is to say one token.
type Profile struct {
	// APIURL is the control plane this profile talks to. Per profile rather
	// than global because a self-hosted deployment and the public one are
	// different accounts on different hosts, and the common mistake is pointing
	// one profile's token at the other's API.
	APIURL string `toml:"api_url"`
	// Account and Organization are cached at login purely so `whoami` and the
	// prompt can say which account a profile is without a round trip. They are
	// a CACHE and are never trusted: the API is asked whenever the answer
	// matters.
	Account      string `toml:"account,omitempty"`
	Organization string `toml:"organization,omitempty"`
}

// Environment variables. The SDK's two are re-exported here rather than
// redefined, because a CLI that invented its own names beside the library's
// would be two answers to "where does the token come from".
const (
	EnvProfile = "FIRSTBOOT_PROFILE"
	EnvConfig  = "FIRSTBOOT_CONFIG"
)

// ErrNoProfile is what every command turns into "run firstboot auth login".
var ErrNoProfile = errors.New("no profile configured")

// ConfigPath is where the file lives. XDG on every platform including macOS: a
// CLI's config is not a macOS application preference, and putting it in
// ~/Library would hide it from the person most likely to want to read it.
func ConfigPath() (string, error) {
	if v := strings.TrimSpace(os.Getenv(EnvConfig)); v != "" {
		return v, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot find a config directory: %w", err)
	}
	return filepath.Join(dir, "firstboot", "config.toml"), nil
}

// LoadConfig reads the file. A missing file is not an error: it is a CLI that
// has not been logged in yet, and every command turns that into the same
// sentence rather than into a stat error.
func LoadConfig() (*Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}
	c := &Config{Profiles: map[string]Profile{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := toml.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if c.Profiles == nil {
		c.Profiles = map[string]Profile{}
	}
	return c, nil
}

// Save writes the file, creating the directory. The file is 0600 even though it
// holds no secret: it names the accounts somebody has, which is not something to
// leave world-readable on a shared machine.
func (c *Config) Save() error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	// Written to a temporary file and renamed, so an interrupted write cannot
	// leave a truncated config that the next run refuses to parse.
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	enc := toml.NewEncoder(f)
	if err := enc.Encode(c); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("encoding the config: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// Names lists the profiles, sorted, for listings and error messages.
func (c *Config) Names() []string {
	out := make([]string, 0, len(c.Profiles))
	for n := range c.Profiles {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Resolve picks the profile a command should use.
//
// The order is: the --profile flag, then FIRSTBOOT_PROFILE, then the configured
// default. An explicit name that does not exist is an ERROR rather than a
// silent fall back to the default, because falling back would run the command
// against the wrong account, and the whole point of profiles is that the wrong
// account is a real hazard.
func (c *Config) Resolve(flag string) (string, Profile, error) {
	name := strings.TrimSpace(flag)
	if name == "" {
		name = strings.TrimSpace(os.Getenv(EnvProfile))
	}
	explicit := name != ""
	if name == "" {
		name = c.DefaultProfile
	}
	if name == "" {
		if len(c.Profiles) == 1 {
			// One profile and no default named: use it. A config with exactly
			// one account has no ambiguity to protect against.
			for n, p := range c.Profiles {
				return n, p, nil
			}
		}
		return "", Profile{}, ErrNoProfile
	}
	p, ok := c.Profiles[name]
	if !ok {
		if explicit {
			return "", Profile{}, fmt.Errorf("no profile called %q. Configured: %s",
				name, strings.Join(c.Names(), ", "))
		}
		return "", Profile{}, fmt.Errorf(
			"the default profile %q is not in the config file. Configured: %s",
			name, strings.Join(c.Names(), ", "))
	}
	return name, p, nil
}
