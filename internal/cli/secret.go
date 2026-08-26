package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/zalando/go-keyring"
)

// Where the token goes, and why not in the config file.
//
// A token acts as its owner inside an organization and can spend the wallet. A
// CLI that writes one into a dotfile has put a credential somewhere a backup
// tool copies, a `cat ~/.config/*` finds, and a screen-share shows. So the
// default is the operating system's own secret store: Keychain on macOS, the
// Secret Service on Linux.
//
// Where there is no store -- a container, a CI runner, a headless box -- there
// is a file, mode 0600, and the CLI SAYS SO at login. That warning is the whole
// point of the fallback: silently degrading to a plaintext file is how somebody
// ends up believing their token is in the Keychain when it is in their home
// directory.
//
// The environment variable outranks both and stores nothing. That is the right
// shape for CI, where the secret arrives from the runner's own store and should
// not be written to the filesystem at all.

// keyringService is the name the OS store lists this CLI under. A literal that
// spells the brand, because it is written OUTSIDE this project into somebody's
// Keychain, where the name is the only clue to who put it there.
const keyringService = "firstboot" // check-brand:allow OS keychain entry name

// SecretStore is where a profile's token lives.
type SecretStore int

const (
	// StoreKeyring is the OS secret store.
	StoreKeyring SecretStore = iota
	// StoreFile is the 0600 fallback.
	StoreFile
	// StoreEnv means the token came from the environment and was never stored.
	StoreEnv
)

func (s SecretStore) String() string {
	switch s {
	case StoreKeyring:
		return keyringName()
	case StoreFile:
		return "a file in the config directory (mode 0600)"
	default:
		return "the environment"
	}
}

func keyringName() string {
	switch runtime.GOOS {
	case "darwin":
		return "the macOS Keychain"
	case "windows":
		return "the Windows Credential Manager"
	default:
		return "the system Secret Service"
	}
}

// TokenFor returns a profile's token and says where it came from.
//
// FIRSTBOOT_TOKEN wins over everything, including an explicitly named profile.
// That is deliberate and it is the CI case: a pipeline sets the variable and
// must not be silently overridden by whatever happens to be on the runner's
// disk.
func TokenFor(profile string) (string, SecretStore, error) {
	if v := strings.TrimSpace(os.Getenv(envToken)); v != "" {
		return v, StoreEnv, nil
	}
	if profile == "" {
		return "", StoreEnv, ErrNoProfile
	}
	if secret, err := keyring.Get(keyringService, profile); err == nil && secret != "" {
		return secret, StoreKeyring, nil
	} else if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		// A keyring that exists but refused is worth saying out loud rather
		// than falling through to the file: on macOS this is the "always allow"
		// prompt being denied, and silently reading a stale file instead would
		// use a token the person thought they had revoked.
		if _, ferr := readTokenFile(profile); ferr == nil {
			return "", StoreKeyring, fmt.Errorf(
				"%s refused to hand over the token for profile %q: %w\n"+
					"There is also a token in the fallback file. Re-run `firstboot auth login "+
					"--profile %s` to decide which one is current",
				keyringName(), profile, err, profile)
		}
	}
	if secret, err := readTokenFile(profile); err == nil && secret != "" {
		return secret, StoreFile, nil
	}
	return "", StoreEnv, ErrNoProfile
}

// StoreToken saves a token and reports where it ended up, so the caller can say
// so. It tries the OS store first and falls back rather than failing: a CLI that
// refused to log in on a headless box would be useless in exactly the place a
// CLI is most useful.
func StoreToken(profile, token string) (SecretStore, error) {
	if err := keyring.Set(keyringService, profile, token); err == nil {
		// Belt and braces: a store that accepted the write and cannot read it
		// back has not stored it, and finding that out now beats finding out on
		// the next command.
		if got, err := keyring.Get(keyringService, profile); err == nil && got == token {
			// Remove any stale fallback file, or a later keyring failure would
			// resurrect a token the person believes they replaced.
			_ = removeTokenFile(profile)
			return StoreKeyring, nil
		}
	}
	if err := writeTokenFile(profile, token); err != nil {
		return StoreFile, err
	}
	return StoreFile, nil
}

// ForgetToken removes a profile's token from wherever it is. Both stores are
// cleared, not the first one that succeeds: a logout that left a copy behind
// would be the worst possible outcome of a command called logout.
func ForgetToken(profile string) error {
	var errs []error
	if err := keyring.Delete(keyringService, profile); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		errs = append(errs, err)
	}
	if err := removeTokenFile(profile); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// tokenFilePath is one file per profile, so a permission problem on one cannot
// take the others with it.
func tokenFilePath(profile string) (string, error) {
	cfg, err := ConfigPath()
	if err != nil {
		return "", err
	}
	// The profile name reaches the filesystem, so it is bounded to what a
	// profile name may be rather than trusted. `../` in a profile name is a
	// path traversal into somebody's home directory.
	if !validProfileName(profile) {
		return "", fmt.Errorf("%q is not a valid profile name: use letters, digits, dashes and underscores", profile)
	}
	return filepath.Join(filepath.Dir(cfg), "credentials", profile+".token"), nil
}

func validProfileName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func readTokenFile(profile string) (string, error) {
	path, err := tokenFilePath(profile)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func writeTokenFile(profile, token string) error {
	path, err := tokenFilePath(profile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// O_TRUNC with 0600 on create, and an explicit Chmod after: a file that
	// already existed keeps its old mode through OpenFile, and the old mode is
	// exactly what a previous version of this CLI might have got wrong.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	_, err = f.WriteString(token + "\n")
	return err
}

func removeTokenFile(profile string) error {
	path, err := tokenFilePath(profile)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

// FallbackWarning is what login prints when the token went to a file. It names
// the path, because "stored in a file" that does not say which file is not a
// warning somebody can act on.
func FallbackWarning(profile string) string {
	path, err := tokenFilePath(profile)
	if err != nil {
		path = "the config directory"
	}
	return fmt.Sprintf(
		"This machine has no usable secret store, so the token was written to\n"+
			"  %s\n"+
			"with mode 0600. Anything running as you can read it. In CI, set %s\n"+
			"from the runner's own secret store instead and it will never touch the disk.",
		path, envToken)
}
