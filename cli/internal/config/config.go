// Package config manages CLI configuration and credential storage.
//
// Tokens are stored in the OS keychain when available (macOS Keychain,
// Linux libsecret, Windows Credential Manager). On headless Linux servers
// where no keyring daemon is running, tokens fall back to
// ~/.cosmos/credentials (chmod 600).
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/zalando/go-keyring"
	"gopkg.in/yaml.v3"
)

const (
	keyringService  = "cosmos-cli"
	configFileName  = "config.yaml"
	credsFileName   = "credentials"
)

// Profile holds the configuration for a single server.
type Profile struct {
	URL  string `yaml:"url"`
	Host string `yaml:"host,omitempty"`
}

// Config holds all CLI configuration.
type Config struct {
	CurrentProfile string             `yaml:"current_profile"`
	Profiles       map[string]Profile `yaml:"profiles"`
}

// Resolved holds the final URL, host header, and token for a request.
type Resolved struct {
	URL   string
	Host  string
	Token string
}

func configDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine home directory: %w", err)
	}
	return filepath.Join(home, ".cosmos"), nil
}

// ConfigPath returns the full path to the config file.
func ConfigPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, configFileName), nil
}

// credsPath returns path to the fallback credentials file.
func credsPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, credsFileName), nil
}

// Load reads the config file. Returns an empty config if the file does not exist.
func Load() (*Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		CurrentProfile: "default",
		Profiles:       make(map[string]Profile),
	}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = make(map[string]Profile)
	}
	return cfg, nil
}

// Save writes the config file, creating ~/.cosmos if needed.
func Save(cfg *Config) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("serialising config: %w", err)
	}

	path := filepath.Join(dir, configFileName)
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return nil
}

// SetToken stores a token — tries OS keychain first, falls back to credentials file.
func SetToken(profile, token string) error {
	if err := keyring.Set(keyringService, profile, token); err == nil {
		return nil
	}
	// Keychain unavailable (headless server) — use credentials file
	return setTokenFile(profile, token)
}

// GetToken retrieves a token — tries OS keychain first, falls back to credentials file.
func GetToken(profile string) (string, error) {
	if token, err := keyring.Get(keyringService, profile); err == nil {
		return token, nil
	}
	// Keychain unavailable — try credentials file
	token, err := getTokenFile(profile)
	if err != nil {
		return "", fmt.Errorf("token not found for profile %q — run 'cosmos configure'", profile)
	}
	return token, nil
}

// DeleteToken removes a token from keychain and credentials file.
func DeleteToken(profile string) error {
	_ = keyring.Delete(keyringService, profile)
	_ = deleteTokenFile(profile)
	return nil
}

// setTokenFile writes a token to ~/.cosmos/credentials in INI format.
func setTokenFile(profile, token string) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	path, _ := credsPath()
	creds := readCredsFile(path)
	creds[profile] = token

	var sb strings.Builder
	for p, t := range creds {
		sb.WriteString(fmt.Sprintf("[%s]\ntoken = %s\n\n", p, t))
	}

	return os.WriteFile(path, []byte(sb.String()), 0600)
}

// getTokenFile reads a token from ~/.cosmos/credentials.
func getTokenFile(profile string) (string, error) {
	path, _ := credsPath()
	creds := readCredsFile(path)
	if token, ok := creds[profile]; ok {
		return token, nil
	}
	return "", fmt.Errorf("not found")
}

// deleteTokenFile removes a profile from ~/.cosmos/credentials.
func deleteTokenFile(profile string) error {
	path, _ := credsPath()
	creds := readCredsFile(path)
	delete(creds, profile)
	var sb strings.Builder
	for p, t := range creds {
		sb.WriteString(fmt.Sprintf("[%s]\ntoken = %s\n\n", p, t))
	}
	return os.WriteFile(path, []byte(sb.String()), 0600)
}

// readCredsFile parses a simple INI credentials file.
func readCredsFile(path string) map[string]string {
	creds := make(map[string]string)
	data, err := os.ReadFile(path)
	if err != nil {
		return creds
	}
	var currentProfile string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			currentProfile = line[1 : len(line)-1]
		} else if strings.HasPrefix(line, "token = ") && currentProfile != "" {
			creds[currentProfile] = strings.TrimPrefix(line, "token = ")
		}
	}
	return creds
}

// hostFromURL extracts the hostname from a URL string.
func hostFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Hostname()
}

// Resolve returns the URL, host header, and token to use for a request.
// Priority: flags > env vars > config file + keychain/credentials file.
func Resolve(profileFlag, urlFlag, tokenFlag string) (*Resolved, error) {
	// 1. Direct flag overrides
	if urlFlag != "" && tokenFlag != "" {
		host := os.Getenv("COSMOS_HOST")
		if host == "" {
			host = hostFromURL(urlFlag)
		}
		return &Resolved{URL: urlFlag, Host: host, Token: tokenFlag}, nil
	}

	// 2. Environment variable overrides — CI / headless servers
	envURL := os.Getenv("COSMOS_URL")
	envToken := os.Getenv("COSMOS_TOKEN")
	envHost := os.Getenv("COSMOS_HOST")

	if envURL != "" && envToken != "" {
		host := envHost
		if host == "" {
			host = hostFromURL(envURL)
		}
		return &Resolved{URL: envURL, Host: host, Token: envToken}, nil
	}

	// 3. Config file + keychain/credentials
	cfg, err := Load()
	if err != nil {
		return nil, err
	}

	profile := profileFlag
	if profile == "" {
		profile = os.Getenv("COSMOS_PROFILE")
	}
	if profile == "" {
		profile = cfg.CurrentProfile
	}
	if profile == "" {
		profile = "default"
	}

	p, ok := cfg.Profiles[profile]
	if !ok && envURL == "" && urlFlag == "" {
		return nil, fmt.Errorf("profile %q not found — run 'cosmos configure'", profile)
	}

	rawURL := urlFlag
	if rawURL == "" { rawURL = envURL }
	if rawURL == "" { rawURL = p.URL }
	if rawURL == "" {
		return nil, fmt.Errorf("no URL configured — run 'cosmos configure' or set COSMOS_URL")
	}

	token := tokenFlag
	if token == "" { token = envToken }
	if token == "" {
		token, err = GetToken(profile)
		if err != nil {
			return nil, err
		}
	}

	host := envHost
	if host == "" {
		host = p.Host
	}
	if host == "" {
		host = hostFromURL(rawURL)
	}

	return &Resolved{URL: rawURL, Host: host, Token: token}, nil
}
