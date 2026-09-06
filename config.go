package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// 2k9s's config lives at ~/.config/2k9s/config.json (XDG-style).
// The binary doesn't depend on any particular source directory or
// organization: everything org-specific lives in this config file, generated
// by `2k9s init`.

const configVersion = 1

type Config struct {
	Version int          `json:"version"`
	Orgs    []*OrgConfig `json:"orgs"`
}

// OrgConfig = one AWS IAM Identity Center instance.
type OrgConfig struct {
	Name           string         `json:"name"`
	SSOStartURL    string         `json:"sso_start_url"`
	SSORegion      string         `json:"sso_region"`
	DefaultRegions []string       `json:"default_regions"`
	LoginProfile   string         `json:"login_profile,omitempty"`
	RolePreference []string       `json:"role_preference,omitempty"`
	ImportedAt     *time.Time     `json:"imported_at,omitempty"`
	Accounts       []AccountEntry `json:"accounts"`
}

type AccountEntry struct {
	Name  string `json:"name"`
	ID    string `json:"id"`
	Email string `json:"email,omitempty"`
}

// ------------------------------------------------------------------ constraints

// Naming rules: names are also used as the AWS profile name, so no whitespace allowed.
var (
	orgNameRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)
	accountNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	accountIDRe   = regexp.MustCompile(`^[0-9]{12}$`)
	regionRe      = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
)

func (o *OrgConfig) Validate() error {
	if !orgNameRe.MatchString(o.Name) {
		return fmt.Errorf("org name %q is invalid: only letters/digits/._- , must start with a letter or digit, max 32 chars", o.Name)
	}
	if !strings.HasPrefix(o.SSOStartURL, "https://") {
		return fmt.Errorf("org %s: sso_start_url must start with https:// (got %q)", o.Name, o.SSOStartURL)
	}
	if !regionRe.MatchString(o.SSORegion) {
		return fmt.Errorf("org %s: sso_region %q doesn't look like an AWS region code", o.Name, o.SSORegion)
	}
	if len(o.DefaultRegions) == 0 {
		return fmt.Errorf("org %s: default_regions must have at least 1 region", o.Name)
	}
	for _, r := range o.DefaultRegions {
		if !regionRe.MatchString(r) {
			return fmt.Errorf("org %s: default_regions contains %q which doesn't look like an AWS region code", o.Name, r)
		}
	}

	seenName := map[string]bool{}
	seenID := map[string]bool{}
	for i, a := range o.Accounts {
		if !accountNameRe.MatchString(a.Name) {
			return fmt.Errorf("org %s account #%d: name %q is invalid (no whitespace allowed, used as the AWS profile name)", o.Name, i+1, a.Name)
		}
		if !accountIDRe.MatchString(a.ID) {
			return fmt.Errorf("org %s account %s: id %q must be exactly 12 digits as a string (keeps leading zeros)", o.Name, a.Name, a.ID)
		}
		if seenName[a.Name] {
			return fmt.Errorf("org %s: duplicate account name %q", o.Name, a.Name)
		}
		if seenID[a.ID] {
			return fmt.Errorf("org %s: duplicate account id %s", o.Name, a.ID)
		}
		seenName[a.Name] = true
		seenID[a.ID] = true
	}
	return nil
}

func (c *Config) Validate() error {
	if c.Version != configVersion {
		return fmt.Errorf("config version %d is not supported (need %d)", c.Version, configVersion)
	}
	if len(c.Orgs) == 0 {
		return fmt.Errorf("config has no orgs yet")
	}
	seen := map[string]bool{}
	for _, o := range c.Orgs {
		if err := o.Validate(); err != nil {
			return err
		}
		key := strings.ToLower(o.Name)
		if seen[key] {
			return fmt.Errorf("duplicate org name %q", o.Name)
		}
		seen[key] = true
	}
	return nil
}

// ------------------------------------------------------------------ paths

// ConfigPath: TWOK9S_CONFIG > $XDG_CONFIG_HOME/2k9s/config.json > ~/.config/2k9s/config.json
func ConfigPath() string {
	if v := os.Getenv("TWOK9S_CONFIG"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "2k9s", "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "2k9s-config.json")
	}
	return filepath.Join(home, ".config", "2k9s", "config.json")
}

func ConfigExists() bool {
	st, err := os.Stat(ConfigPath())
	return err == nil && !st.IsDir() && st.Size() > 0
}

func LoadConfig() (*Config, error) {
	path := ConfigPath()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no config yet at %s — run `2k9s init` to import accounts", path)
		}
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func SaveConfig(c *Config) error {
	c.Version = configVersion
	if err := c.Validate(); err != nil {
		return err
	}
	path := ConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	// back up the previous version before writing
	if old, err := os.ReadFile(path); err == nil && len(old) > 0 {
		bak := fmt.Sprintf("%s.bak-%s", path, time.Now().Format("20060102-150405"))
		if err := os.WriteFile(bak, old, 0o600); err != nil {
			return fmt.Errorf("couldn't create backup %s: %w", bak, err)
		}
	}

	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	// write to a temp file then rename -> never leaves a half-written config behind
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ------------------------------------------------------------------ utilities

func (c *Config) FindOrg(name string) *OrgConfig {
	for _, o := range c.Orgs {
		if strings.EqualFold(o.Name, name) {
			return o
		}
	}
	return nil
}

func (c *Config) UpsertOrg(n *OrgConfig) {
	for i, o := range c.Orgs {
		if strings.EqualFold(o.Name, n.Name) {
			c.Orgs[i] = n
			return
		}
	}
	c.Orgs = append(c.Orgs, n)
}

// Runtime returns a flat list of Accounts for the rest of the tool to use.
// orgFilter matches the org name exactly (case-insensitive); empty = all.
func (c *Config) Runtime(orgFilter string) ([]Account, error) {
	var out []Account
	for _, oc := range c.Orgs {
		if orgFilter != "" && !strings.EqualFold(oc.Name, orgFilter) {
			continue
		}
		org := &Org{
			Label:          oc.Name,
			StartURL:       oc.SSOStartURL,
			SSORegion:      oc.SSORegion,
			DefaultRegions: oc.DefaultRegions,
			LoginProfile:   oc.LoginProfile,
			RolePreference: oc.RolePreference,
		}
		for _, a := range oc.Accounts {
			out = append(out, Account{Name: a.Name, ID: a.ID, Email: a.Email, Org: org})
		}
	}
	if len(out) == 0 {
		if orgFilter != "" {
			var names []string
			for _, o := range c.Orgs {
				names = append(names, o.Name)
			}
			return nil, fmt.Errorf("no org named %q (available: %s)", orgFilter, strings.Join(names, ", "))
		}
		return nil, fmt.Errorf("config has no accounts — run `2k9s sync`")
	}
	return out, nil
}

// sanitizeName turns an SSO account name into something usable as an AWS profile name.
// Example: "Workload Prod (AWS)" -> "workload-prod-aws"
func sanitizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteRune('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-._")
	if out == "" {
		out = "account"
	}
	if len(out) > 64 {
		out = strings.Trim(out[:64], "-._")
	}
	return out
}
