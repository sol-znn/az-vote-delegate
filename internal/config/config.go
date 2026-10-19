package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zenon-network/go-zenon/common/types"
)

type Delegation struct {
	PillarName       string `json:"pillarName"`
	Follow           string `json:"follow"`
	ProducerKeyFile  string `json:"producerKeyFile"`
	ProducerKeyIndex int    `json:"producerKeyIndex"`
	PasswordEnv      string `json:"passwordEnv,omitempty"`
	PasswordFile     string `json:"passwordFile,omitempty"`
}

type Config struct {
	CopyDelayMinutes    int          `json:"copyDelayMinutes"`
	Live                bool         `json:"live"`
	NodeURL             string       `json:"nodeUrl"`
	ChainID             uint64       `json:"chainId"`
	PollIntervalSeconds int          `json:"pollIntervalSeconds"`
	PowTimeoutSeconds   int          `json:"powTimeoutSeconds"`
	StatePath           string       `json:"statePath"`
	Delegations         []Delegation `json:"delegations"`
}

// FollowAddress reports whether follow names an address rather than a pillar.
// Any address can send a vote block, so an address's votes can be copied too.
func FollowAddress(follow string) (types.Address, bool) {
	address, err := types.ParseAddress(follow)
	return address, err == nil
}

func ValidateCopyDelay(minutes int) error {
	if minutes < 0 || minutes > 10080 {
		return errors.New("copyDelayMinutes must be 0..10080 (up to 7 days)")
	}
	return nil
}
func (c Config) CopyDelay() time.Duration { return time.Duration(c.CopyDelayMinutes) * time.Minute }

func (c Config) PollInterval() time.Duration {
	return time.Duration(c.PollIntervalSeconds) * time.Second
}
func (c Config) PowTimeout() time.Duration { return time.Duration(c.PowTimeoutSeconds) * time.Second }

func Load(path string) (Config, error) {
	c := Config{PollIntervalSeconds: 30, PowTimeoutSeconds: 300, StatePath: "state.json"}
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(io.LimitReader(f, 1<<20))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&c); err != nil {
		return c, fmt.Errorf("config: %w", err)
	}
	var extra any
	if err = dec.Decode(&extra); err != io.EOF {
		return c, errors.New("config must contain one JSON object")
	}
	u, err := url.Parse(c.NodeURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "ws" && u.Scheme != "wss" && u.Scheme != "http" && u.Scheme != "https") {
		return c, errors.New("nodeUrl must be an HTTP(S) or WS(S) URL")
	}
	if c.ChainID == 0 {
		return c, errors.New("chainId must be explicitly set and positive")
	}
	if c.PollIntervalSeconds < 1 || c.PollIntervalSeconds > 86400 || c.PowTimeoutSeconds < 1 || c.PowTimeoutSeconds > 3600 {
		return c, errors.New("pollIntervalSeconds must be 1..86400; powTimeoutSeconds must be 1..3600")
	}
	if err := ValidateCopyDelay(c.CopyDelayMinutes); err != nil {
		return c, err
	}
	if c.StatePath == "" || len(c.Delegations) == 0 {
		return c, errors.New("statePath and at least one delegation are required")
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return c, err
	}
	resolve := func(p string) string {
		if filepath.IsAbs(p) {
			return filepath.Clean(p)
		}
		return filepath.Join(base, p)
	}
	c.StatePath = resolve(c.StatePath)
	seen := map[string]bool{}
	for i := range c.Delegations {
		d := &c.Delegations[i]
		if strings.TrimSpace(d.PillarName) == "" || strings.TrimSpace(d.Follow) == "" || d.ProducerKeyFile == "" {
			return c, fmt.Errorf("delegation %d: pillarName, follow and producerKeyFile are required", i)
		}
		if seen[d.PillarName] {
			return c, fmt.Errorf("duplicate delegation for %s", d.PillarName)
		}
		seen[d.PillarName] = true
		if d.PillarName == d.Follow {
			return c, fmt.Errorf("%s cannot follow itself", d.PillarName)
		}
		if d.ProducerKeyIndex < 0 || d.ProducerKeyIndex > 1000000 {
			return c, errors.New("producerKeyIndex out of range")
		}
		if (d.PasswordEnv == "") == (d.PasswordFile == "") {
			return c, fmt.Errorf("%s: set exactly one of passwordEnv or passwordFile", d.PillarName)
		}
		d.ProducerKeyFile = resolve(d.ProducerKeyFile)
		if d.PasswordFile != "" {
			d.PasswordFile = resolve(d.PasswordFile)
		}
		if c.StatePath == d.ProducerKeyFile || c.StatePath == d.PasswordFile {
			return c, errors.New("statePath must not name a key or password file")
		}
	}
	configPath, err := filepath.Abs(path)
	if err != nil {
		return c, err
	}
	if c.StatePath == configPath {
		return c, errors.New("statePath must not name the config file")
	}
	return c, nil
}

func Password(d Delegation) (string, error) {
	if d.PasswordEnv != "" {
		value, ok := os.LookupEnv(d.PasswordEnv)
		if !ok || value == "" {
			return "", fmt.Errorf("password environment variable %s is unset or empty", d.PasswordEnv)
		}
		return value, nil
	}
	info, err := os.Lstat(d.PasswordFile)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return "", errors.New("passwordFile must be a regular file with mode 0600")
	}
	f, err := os.Open(d.PasswordFile)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(info, opened) || opened.Mode().Perm() != 0600 {
		return "", errors.New("passwordFile changed while opening")
	}
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return "", err
	}
	if len(b) > 65536 {
		return "", errors.New("passwordFile too large")
	}
	password := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if password == "" {
		return "", errors.New("passwordFile is empty")
	}
	return password, nil
}
