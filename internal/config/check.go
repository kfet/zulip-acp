package config

import (
	"errors"
	"os"
	"path/filepath"
)

// Env names that override config fields, so the API key never has to
// live in a config file on the host.
const (
	EnvSite   = "ZULIP_SITE"
	EnvEmail  = "ZULIP_EMAIL"
	EnvAPIKey = "ZULIP_API_KEY"
)

// ApplyEnv applies the credential environment overrides.
func (c *Config) ApplyEnv(getenv func(string) string) {
	if v := getenv(EnvSite); v != "" {
		c.Site = v
	}
	if v := getenv(EnvEmail); v != "" {
		c.BotEmail = v
	}
	if v := getenv(EnvAPIKey); v != "" {
		c.BotAPIKey = v
	}
}

// Check runs every check the relay can do before it reaches the
// server: the field checks of Validate, the credentials, and that the
// delete key file is readable with safe permissions. It never returns
// a key. The server-side checks (authentication, channel names, the
// delete bot's login) run only at start.
func (c *Config) Check() error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := ValidateCredentials(c.Site, c.BotEmail, c.BotAPIKey); err != nil {
		return err
	}
	if c.Delete != nil {
		if _, err := c.Delete.ReadDeleteKey(); err != nil {
			return err
		}
	}
	return nil
}

// CheckFile loads path, applies the environment and runs Check. It is
// the ONE validation that `check-config`, a reload and a start share,
// so the three cannot disagree about a file.
func CheckFile(path string, getenv func(string) string) (*Config, error) {
	c, err := Load(path)
	if err != nil {
		return nil, err
	}
	c.ApplyEnv(getenv)
	if err := c.Check(); err != nil {
		return nil, err
	}
	return c, nil
}

// LastGoodPath is where the relay keeps a copy of the last config file
// that passed CheckFile. It sits next to the config, so it is found
// without reading the config.
func LastGoodPath(path string) string { return path + ".last-good" }

// SaveLastGood copies the config file at path to LastGoodPath(path),
// atomically and readable only by the owner (a config can hold an API
// key).
func SaveLastGood(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dst := LastGoodPath(path)
	tmp, err := createTemp(filepath.Dir(dst), ".last-good-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// createTemp is os.CreateTemp, a seam for the write-error test.
var createTemp = os.CreateTemp

// LoadOrLastGood is the start-up load. It returns the config at path
// when it passes CheckFile, and saves it as the last good copy. When it
// fails and a last good copy passes, it returns that copy and, as
// fellBack, why path was refused: the relay then runs instead of
// crash-looping, and the caller must report fellBack loudly. With no
// usable copy it returns the error for path. A failure to save the
// copy only costs the fallback for a later start, so it is logged.
func LoadOrLastGood(path string, getenv func(string) string, logf func(string, ...any)) (c *Config, fellBack error, err error) {
	c, err = CheckFile(path, getenv)
	if err == nil {
		if serr := SaveLastGood(path); serr != nil {
			logf("config: WARN cannot save the last good copy: %v", serr)
		}
		return c, nil, nil
	}
	lg, lerr := CheckFile(LastGoodPath(path), getenv)
	if lerr != nil {
		return nil, nil, err
	}
	return lg, err, nil
}
