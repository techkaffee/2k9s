package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// All AWS interaction goes through the `aws` CLI v2 (already on the machine)
// instead of the AWS SDK, so the binary needs no external dependency and
// shares the SSO cache with the aws cli.

type Creds struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expiration      time.Time
}

// ---------------------------------------------------------------- aws helpers

func awsRun(env []string, args ...string) ([]byte, error) {
	cmd := exec.Command("aws", args...)
	if env != nil {
		cmd.Env = env
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("aws %s: %s", strings.Join(redactArgs(args), " "), msg)
	}
	return stdout.Bytes(), nil
}

// redactArgs hides the access token when printing errors to the terminal
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i := 0; i < len(out)-1; i++ {
		if out[i] == "--access-token" {
			out[i+1] = "***"
		}
	}
	return out
}

func awsJSON(env []string, out any, args ...string) error {
	b, err := awsRun(env, args...)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// baseEnv returns os.Environ() with any credential/profile variables removed,
// so an active profile can't override the temporary credentials we pass in.
func baseEnv() []string {
	drop := map[string]bool{
		"AWS_PROFILE": true, "AWS_DEFAULT_PROFILE": true,
		"AWS_ACCESS_KEY_ID": true, "AWS_SECRET_ACCESS_KEY": true,
		"AWS_SESSION_TOKEN": true, "AWS_SECURITY_TOKEN": true,
		"AWS_REGION": true, "AWS_DEFAULT_REGION": true,
	}
	var out []string
	for _, kv := range os.Environ() {
		i := strings.Index(kv, "=")
		if i > 0 && drop[kv[:i]] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func envWithCreds(c Creds, region string) []string {
	env := baseEnv()
	env = append(env,
		"AWS_ACCESS_KEY_ID="+c.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY="+c.SecretAccessKey,
		"AWS_SESSION_TOKEN="+c.SessionToken,
	)
	if region != "" {
		env = append(env, "AWS_REGION="+region, "AWS_DEFAULT_REGION="+region)
	}
	return env
}

func envWithProfile(profile, region string) []string {
	env := append(baseEnv(), "AWS_PROFILE="+profile)
	if region != "" {
		env = append(env, "AWS_REGION="+region, "AWS_DEFAULT_REGION="+region)
	}
	return env
}

// ------------------------------------------------------------------ SSO token

func normURL(u string) string { return strings.TrimRight(strings.TrimSpace(u), "/") }

func parseAWSTime(s string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05Z0700",
		"2006-01-02T15:04:05.999Z0700",
		"2006-01-02T15:04:05UTC",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("couldn't parse time %q", s)
}

// findSSOToken scans ~/.aws/sso/cache for a still-valid access token for the start URL.
func findSSOToken(startURL string) (string, time.Time, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", time.Time{}, false
	}
	entries, err := os.ReadDir(filepath.Join(home, ".aws", "sso", "cache"))
	if err != nil {
		return "", time.Time{}, false
	}

	var bestTok string
	var bestExp time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(home, ".aws", "sso", "cache", e.Name()))
		if err != nil {
			continue
		}
		var t struct {
			StartURL    string `json:"startUrl"`
			AccessToken string `json:"accessToken"`
			ExpiresAt   string `json:"expiresAt"`
		}
		if json.Unmarshal(b, &t) != nil || t.AccessToken == "" {
			continue
		}
		if normURL(t.StartURL) != normURL(startURL) {
			continue
		}
		exp, err := parseAWSTime(t.ExpiresAt)
		if err != nil || exp.Before(time.Now().Add(60*time.Second)) {
			continue
		}
		if exp.After(bestExp) {
			bestExp, bestTok = exp, t.AccessToken
		}
	}
	return bestTok, bestExp, bestTok != ""
}

// EnsureLoginProfile returns the profile used for `aws sso login`.
// If the org hasn't configured a login_profile, it creates a minimal
// `2k9s-login-<org>` profile with just sso_start_url + sso_region — enough
// to log in. This way the tool doesn't depend on any org's pre-existing profile.
func EnsureLoginProfile(org *Org) (string, error) {
	if org.LoginProfile != "" && ProfileExists(org.LoginProfile) {
		return org.LoginProfile, nil
	}
	name := org.LoginProfile
	if name == "" {
		name = "2k9s-login-" + org.Label
	}
	if ProfileExists(name) {
		org.LoginProfile = name
		return name, nil
	}
	body := fmt.Sprintf("sso_start_url = %s\nsso_region = %s\n", org.StartURL, org.SSORegion)
	if err := appendAWSConfigBlock(name, body); err != nil {
		return "", err
	}
	okf("added [profile %s] to ~/.aws/config (SSO login only)", name)
	org.LoginProfile = name
	return name, nil
}

// EnsureSSOToken gets a token, running `aws sso login` if it has expired.
// If forceLogin is set, the local cache is skipped and a fresh login is run
// even if a cached token looks unexpired (used to recover from a token that
// the cache says is valid but that AWS has actually revoked/invalidated).
func EnsureSSOToken(org *Org, allowLogin bool) (string, error) {
	return ensureSSOToken(org, allowLogin, false)
}

func ensureSSOToken(org *Org, allowLogin, forceLogin bool) (string, error) {
	if !forceLogin {
		if tok, exp, ok := findSSOToken(org.StartURL); ok {
			infof("SSO token %s still valid until %s", org.Label, exp.Local().Format("15:04 02/01"))
			return tok, nil
		}
	}
	if !allowLogin {
		hint := org.LoginProfile
		if hint == "" {
			hint = "<this org's profile>"
		}
		return "", fmt.Errorf("SSO token for %s has expired, run: aws sso login --profile %s",
			org.StartURL, hint)
	}

	profile, err := EnsureLoginProfile(org)
	if err != nil {
		return "", err
	}

	if forceLogin {
		warnf("SSO token %s was rejected by AWS -> aws sso login --profile %s (opening browser)", org.Label, profile)
	} else {
		warnf("SSO token %s expired -> aws sso login --profile %s (opening browser)", org.Label, profile)
	}
	cmd := exec.Command("aws", "sso", "login", "--profile", profile)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("aws sso login failed (profile %s): %w", profile, err)
	}
	tok, _, ok := findSSOToken(org.StartURL)
	if !ok {
		return "", fmt.Errorf("login finished but couldn't read a token from ~/.aws/sso/cache")
	}
	return tok, nil
}

// isSSOUnauthorized reports whether err is the SSO API rejecting an access
// token as invalid/expired server-side (UnauthorizedException / "Session
// token not found or invalid"), even though our local cache thought the
// token was still valid (e.g. it was revoked, or the cache file is stale).
func isSSOUnauthorized(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UnauthorizedException") ||
		strings.Contains(msg, "Session token not found or invalid")
}

// ForceSSOLogin re-runs `aws sso login` unconditionally, ignoring any cached
// token, and returns the fresh token. Used to recover when AWS rejects a
// token that the local cache believed was still valid.
func ForceSSOLogin(org *Org, allowLogin bool) (string, error) {
	if !allowLogin {
		hint := org.LoginProfile
		if hint == "" {
			hint = "<this org's profile>"
		}
		return "", fmt.Errorf("SSO token for %s was rejected by AWS, run: aws sso login --profile %s",
			org.StartURL, hint)
	}
	return ensureSSOToken(org, true, true)
}

// ListSSOAccounts fetches all accounts the user is allowed to access in the SSO instance.
// This is 2k9s's main account source (the aws cli paginates automatically).
func ListSSOAccounts(token, ssoRegion string) ([]AccountEntry, error) {
	var resp struct {
		AccountList []struct {
			AccountID    string `json:"accountId"`
			AccountName  string `json:"accountName"`
			EmailAddress string `json:"emailAddress"`
		} `json:"accountList"`
	}
	err := awsJSON(baseEnv(), &resp,
		"sso", "list-accounts",
		"--access-token", token,
		"--region", ssoRegion,
		"--output", "json",
	)
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var out []AccountEntry
	for _, a := range resp.AccountList {
		if !accountIDRe.MatchString(a.AccountID) {
			continue
		}
		name := sanitizeName(a.AccountName)
		// account names in SSO can collide after sanitizing -> add a suffix
		base := name
		for i := 2; seen[name]; i++ {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		seen[name] = true
		out = append(out, AccountEntry{Name: name, ID: a.AccountID, Email: a.EmailAddress})
	}
	return out, nil
}

// defaultRolePreference: common AWS Identity Center role names.
// An org that uses custom role names should set "role_preference" in config.
var defaultRolePreference = []string{
	"AWSAdministratorAccess",
	"AWSPowerUserAccess",
	"AWSReadOnlyAccess",
	"AWSViewOnlyAccess",
	"ViewOnlyAccess",
	"ReadOnlyAccess",
}

func ListAccountRoles(token, accountID, ssoRegion string) ([]string, error) {
	var resp struct {
		RoleList []struct {
			RoleName string `json:"roleName"`
		} `json:"roleList"`
	}
	err := awsJSON(baseEnv(), &resp,
		"sso", "list-account-roles",
		"--account-id", accountID,
		"--access-token", token,
		"--region", ssoRegion,
		"--output", "json",
	)
	if err != nil {
		return nil, err
	}
	var roles []string
	for _, r := range resp.RoleList {
		roles = append(roles, r.RoleName)
	}
	return roles, nil
}

// PickRole picks a role using the org's role_preference, falling back to the
// default list, and finally to the first role the user has.
func PickRole(roles []string, preference []string) string {
	if len(roles) == 0 {
		return ""
	}
	available := map[string]bool{}
	for _, r := range roles {
		available[r] = true
	}
	for _, list := range [][]string{preference, defaultRolePreference} {
		for _, p := range list {
			if available[p] {
				return p
			}
		}
	}
	return roles[0]
}

func GetRoleCredentials(token, accountID, role, ssoRegion string) (Creds, error) {
	var resp struct {
		RoleCredentials struct {
			AccessKeyID     string `json:"accessKeyId"`
			SecretAccessKey string `json:"secretAccessKey"`
			SessionToken    string `json:"sessionToken"`
			Expiration      int64  `json:"expiration"` // epoch millis
		} `json:"roleCredentials"`
	}
	err := awsJSON(baseEnv(), &resp,
		"sso", "get-role-credentials",
		"--account-id", accountID,
		"--role-name", role,
		"--access-token", token,
		"--region", ssoRegion,
		"--output", "json",
	)
	if err != nil {
		return Creds{}, err
	}
	rc := resp.RoleCredentials
	if rc.AccessKeyID == "" || rc.SecretAccessKey == "" || rc.SessionToken == "" {
		return Creds{}, fmt.Errorf("get-role-credentials returned empty credentials")
	}
	return Creds{
		AccessKeyID:     rc.AccessKeyID,
		SecretAccessKey: rc.SecretAccessKey,
		SessionToken:    rc.SessionToken,
		Expiration:      time.UnixMilli(rc.Expiration),
	}, nil
}

// ------------------------------------------------------------ ~/.aws/config

func awsConfigPath() (string, error) {
	if v := os.Getenv("AWS_CONFIG_FILE"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".aws", "config"), nil
}

// ProfileExists checks whether a [profile <name>] block already exists in ~/.aws/config.
func ProfileExists(name string) bool {
	path, err := awsConfigPath()
	if err != nil {
		return false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	re := regexp.MustCompile(`(?m)^\s*\[\s*profile\s+` + regexp.QuoteMeta(name) + `\s*\]\s*$`)
	return re.Match(b)
}

// appendAWSConfigBlock appends a [profile name] block to ~/.aws/config,
// backing it up first. Doesn't modify an existing block.
func appendAWSConfigBlock(name, body string) error {
	path, err := awsConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	if old, err := os.ReadFile(path); err == nil && len(old) > 0 {
		bak := fmt.Sprintf("%s.bak-2k9s-%s", path, time.Now().Format("20060102-150405"))
		if err := os.WriteFile(bak, old, 0o600); err != nil {
			return fmt.Errorf("couldn't create backup %s: %w", bak, err)
		}
		infof("backed up ~/.aws/config -> %s", filepath.Base(bak))
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "\n[profile %s]\n%s", name, body)
	return err
}

// EnsureProfile creates an SSO profile for the account if it doesn't already exist.
// Idempotent: if the profile already exists, nothing is changed (respects existing config).
func EnsureProfile(acct Account, role, region string) (created bool, err error) {
	if ProfileExists(acct.Name) {
		return false, nil
	}
	body := fmt.Sprintf(`sso_start_url = %s
sso_region = %s
sso_account_id = %s
sso_role_name = %s
region = %s
output = json
`, acct.Org.StartURL, acct.Org.SSORegion, acct.ID, role, region)

	if err := appendAWSConfigBlock(acct.Name, body); err != nil {
		return false, err
	}
	return true, nil
}

// ----------------------------------------------------------------------- EKS

func ListClusters(env []string, region string) ([]string, error) {
	var resp struct {
		Clusters []string `json:"clusters"`
	}
	err := awsJSON(env, &resp, "eks", "list-clusters", "--region", region, "--output", "json")
	if err != nil {
		return nil, err
	}
	return resp.Clusters, nil
}

func UpdateKubeconfig(profile, region, cluster, alias string) error {
	_, err := awsRun(envWithProfile(profile, region),
		"eks", "update-kubeconfig",
		"--region", region,
		"--name", cluster,
		"--alias", alias,
		"--profile", profile,
	)
	return err
}

// ---------------------------------------------------------------- kubeconfig

type KContext struct {
	Name      string
	ClusterID string // cluster ref in kubeconfig (usually an ARN)
	AccountID string
	Region    string
	Cluster   string
	Current   bool
}

var eksARNRe = regexp.MustCompile(`^arn:aws[\w-]*:eks:([a-z0-9-]+):(\d{12}):cluster/(.+)$`)

func ListKubeContexts() ([]KContext, error) {
	cmd := exec.Command("kubectl", "config", "view", "-o", "json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("kubectl config view: %s", strings.TrimSpace(stderr.String()))
	}
	var cfg struct {
		CurrentContext string `json:"current-context"`
		Contexts       []struct {
			Name    string `json:"name"`
			Context struct {
				Cluster string `json:"cluster"`
			} `json:"context"`
		} `json:"contexts"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &cfg); err != nil {
		return nil, err
	}

	var out []KContext
	for _, c := range cfg.Contexts {
		k := KContext{
			Name:      c.Name,
			ClusterID: c.Context.Cluster,
			Current:   c.Name == cfg.CurrentContext,
		}
		if m := eksARNRe.FindStringSubmatch(c.Context.Cluster); m != nil {
			k.Region, k.AccountID, k.Cluster = m[1], m[2], m[3]
		}
		out = append(out, k)
	}
	return out, nil
}
