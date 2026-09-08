package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Commands that manage the account source: init / sync / import / accounts / config.

// ------------------------------------------------------------------- prompts

var stdin = bufio.NewReader(os.Stdin)

func prompt(question, def string) (string, error) {
	if !isTTY(os.Stdin) {
		if def == "" {
			return "", fmt.Errorf("need to enter %q but stdin isn't a terminal", question)
		}
		return def, nil
	}
	if def != "" {
		fmt.Fprintf(os.Stderr, "%s [%s]: ", question, def)
	} else {
		fmt.Fprintf(os.Stderr, "%s: ", question)
	}
	line, err := stdin.ReadString('\n')
	if err != nil && line == "" {
		return "", errNoSelection
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

// --------------------------------------------------- discover SSO instance

type ssoCandidate struct {
	StartURL string
	Region   string
	Source   string
}

var (
	reStartURL   = regexp.MustCompile(`(?m)^\s*sso_start_url\s*=\s*(\S+)`)
	reSSORegion  = regexp.MustCompile(`(?m)^\s*sso_region\s*=\s*(\S+)`)
	reConfigHead = regexp.MustCompile(`(?m)^\s*\[[^\]]+\]\s*$`)
)

// discoverSSO collects the SSO instances already seen on this machine: from
// ~/.aws/sso/cache and from ~/.aws/config. This usually means `init` doesn't
// need the user to type a URL by hand.
func discoverSSO() []ssoCandidate {
	seen := map[string]*ssoCandidate{}
	add := func(url, region, src string) {
		url = normURL(url)
		if url == "" {
			return
		}
		if c, ok := seen[url]; ok {
			if c.Region == "" {
				c.Region = region
			}
			return
		}
		seen[url] = &ssoCandidate{StartURL: url, Region: region, Source: src}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	// 1) token cache
	dir := filepath.Join(home, ".aws", "sso", "cache")
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			var t struct {
				StartURL string `json:"startUrl"`
				Region   string `json:"region"`
			}
			if json.Unmarshal(b, &t) == nil && t.StartURL != "" {
				add(t.StartURL, t.Region, "sso cache")
			}
		}
	}

	// 2) ~/.aws/config — split by each [..] block so the url is paired with the right region
	if path, err := awsConfigPath(); err == nil {
		if b, err := os.ReadFile(path); err == nil {
			text := string(b)
			idx := reConfigHead.FindAllStringIndex(text, -1)
			for i, loc := range idx {
				end := len(text)
				if i+1 < len(idx) {
					end = idx[i+1][0]
				}
				block := text[loc[0]:end]
				mu := reStartURL.FindStringSubmatch(block)
				if mu == nil {
					continue
				}
				region := ""
				if mr := reSSORegion.FindStringSubmatch(block); mr != nil {
					region = mr[1]
				}
				add(mu[1], region, "~/.aws/config")
			}
		}
	}

	var out []ssoCandidate
	for _, c := range seen {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartURL < out[j].StartURL })
	return out
}

// orgNameFromURL: https://d-966703e67e.awsapps.com/start -> d-966703e67e
func orgNameFromURL(u string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(normURL(u), "https://"), "http://")
	if i := strings.Index(s, "."); i > 0 {
		s = s[:i]
	}
	s = strings.TrimSuffix(s, "-sso-portal")
	s = strings.TrimSuffix(s, "-sso")
	return sanitizeName(s)
}

// ------------------------------------------------------------------ init

func cmdInit(args []string) error {
	o := options{}
	fs := newFlagSet("init", "import accounts from AWS SSO into config",
		`Automatically finds SSO instances in ~/.aws/sso/cache and ~/.aws/config for
you to pick from, asks for an org name + regions to search for clusters, then
calls "aws sso list-accounts" to fetch exactly the accounts you have access
to. Run it multiple times for multiple organizations.`)
	var force bool
	fs.BoolVar(&force, "force", false, "overwrite the org already in config")
	fs.BoolVar(&o.assumeYes, "y", false, "don't prompt, use default values")
	fs.BoolVar(&o.noInstall, "no-install", false, "don't auto-install tools")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := EnsureTools(options{noInstall: o.noInstall, assumeYes: o.assumeYes}); err != nil {
		return err
	}

	cfg := &Config{Version: configVersion}
	if ConfigExists() {
		if c, err := LoadConfig(); err == nil {
			cfg = c
		}
	}

	// 1) pick an SSO instance
	cands := discoverSSO()
	var startURL, ssoRegion string
	if len(cands) > 0 {
		items := make([]Item, 0, len(cands)+1)
		for _, c := range cands {
			r := c.Region
			if r == "" {
				r = "?"
			}
			items = append(items, Item{Key: c.StartURL + "|" + c.Region, Cols: []string{c.StartURL, r, c.Source}})
		}
		items = append(items, Item{Key: "__manual__", Cols: []string{"(type a different URL)", "", ""}})

		sel, err := Pick("sso instance", "pick an AWS IAM Identity Center instance", items)
		if err != nil {
			return err
		}
		if sel != "__manual__" {
			parts := strings.SplitN(sel, "|", 2)
			startURL, ssoRegion = parts[0], parts[1]
		}
	}
	if startURL == "" {
		v, err := prompt("SSO start URL (e.g. https://my-sso.awsapps.com/start)", "")
		if err != nil {
			return err
		}
		startURL = normURL(v)
	}
	if !strings.HasPrefix(startURL, "https://") {
		return fmt.Errorf("sso start url must start with https:// (got %q)", startURL)
	}
	if ssoRegion == "" {
		v, err := prompt("SSO region", "us-east-1")
		if err != nil {
			return err
		}
		ssoRegion = v
	}

	// 2) org name
	defName := orgNameFromURL(startURL)
	orgName, err := prompt("Org name (used for --org)", defName)
	if err != nil {
		return err
	}
	orgName = sanitizeName(orgName)
	if existing := cfg.FindOrg(orgName); existing != nil && !force {
		return fmt.Errorf("org %q already exists in config. Use `2k9s sync --org %s` to refresh accounts, or `2k9s init --force` to overwrite the org's config",
			orgName, orgName)
	}

	// 3) regions to search for clusters
	regStr, err := prompt("Region(s) to search for EKS clusters (comma-separated)", ssoRegion)
	if err != nil {
		return err
	}
	regions := splitCSV(regStr)

	oc := &OrgConfig{
		Name:           orgName,
		SSOStartURL:    startURL,
		SSORegion:      ssoRegion,
		DefaultRegions: regions,
	}
	if old := cfg.FindOrg(orgName); old != nil {
		oc.LoginProfile = old.LoginProfile
		oc.RolePreference = old.RolePreference
		oc.Accounts = old.Accounts
	}

	// 4) import accounts from SSO
	added, err := syncOrg(oc)
	if err != nil {
		return err
	}
	cfg.UpsertOrg(oc)
	if err := SaveConfig(cfg); err != nil {
		return err
	}

	okf("saved %s", ConfigPath())
	okf("org %s: %d accounts (%d new)", oc.Name, len(oc.Accounts), added)
	infof("run `2k9s` to pick an account and open k9s")
	return nil
}

// ------------------------------------------------------------------ sync

func cmdSync(args []string) error {
	var orgFilter string
	var noInstall, yes bool
	fs := newFlagSet("sync", "refresh the account list from AWS SSO",
		`Merges by account ID: new accounts are added, existing accounts keep the
name you've edited by hand, and accounts no longer in SSO are only warned
about, never deleted.`)
	fs.StringVar(&orgFilter, "org", "", "only sync one org")
	fs.BoolVar(&noInstall, "no-install", false, "don't auto-install tools")
	fs.BoolVar(&yes, "y", false, "don't prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := EnsureTools(options{noInstall: noInstall, assumeYes: yes}); err != nil {
		return err
	}

	cfg, err := LoadConfig()
	if err != nil {
		return err
	}

	total := 0
	for _, oc := range cfg.Orgs {
		if orgFilter != "" && !strings.EqualFold(oc.Name, orgFilter) {
			continue
		}
		added, err := syncOrg(oc)
		if err != nil {
			warnf("org %s: %v", oc.Name, err)
			continue
		}
		total++
		okf("org %s: %d accounts (%d new)", oc.Name, len(oc.Accounts), added)
	}
	if total == 0 {
		return fmt.Errorf("no org was synced")
	}
	return SaveConfig(cfg)
}

// syncOrg calls `aws sso list-accounts` then merges the result into oc.Accounts.
func syncOrg(oc *OrgConfig) (added int, err error) {
	org := &Org{
		Label: oc.Name, StartURL: oc.SSOStartURL,
		SSORegion: oc.SSORegion, LoginProfile: oc.LoginProfile,
	}
	token, err := EnsureSSOToken(org, true)
	if err != nil {
		return 0, err
	}
	// EnsureSSOToken may have created a minimal login profile -> remember it in config
	if oc.LoginProfile == "" && org.LoginProfile != "" {
		oc.LoginProfile = org.LoginProfile
	}

	fresh, err := ListSSOAccounts(token, oc.SSORegion)
	if isSSOUnauthorized(err) {
		// the cache said the token was still valid, but AWS rejected it
		// (revoked/invalidated) -> force a fresh login and retry once.
		token, err = ForceSSOLogin(org, true)
		if err == nil {
			fresh, err = ListSSOAccounts(token, oc.SSORegion)
		}
	}
	if err != nil {
		return 0, err
	}
	if len(fresh) == 0 {
		return 0, fmt.Errorf("sso list-accounts returned no accounts")
	}

	merged, addedList, removed := mergeAccounts(oc.Accounts, fresh)
	sort.Slice(merged, func(i, j int) bool { return merged[i].Name < merged[j].Name })
	oc.Accounts = merged
	now := time.Now().UTC()
	oc.ImportedAt = &now

	for _, a := range addedList {
		infof("  + %s (%s)", a.Name, a.ID)
	}
	for _, a := range removed {
		warnf("  - %s (%s) no longer seen in SSO, keeping it in config", a.Name, a.ID)
	}
	return len(addedList), nil
}

// ------------------------------------------------------------------ import

func cmdImport(args []string) error {
	var file, orgName string
	fs := newFlagSet("import", "import accounts from a flat file",
		`File format:
  # org: acme
  # sso-start-url: https://acme.awsapps.com/start
  # sso-region: ap-southeast-1
  # default-regions: ap-southeast-1,us-east-1
  workload-prod,230173707921,workload-prod@acme.com

Data lines are name,id[,email]. id must be exactly 12 digits.`)
	fs.StringVar(&file, "file", "", "the .txt file to import (required)")
	fs.StringVar(&orgName, "org", "", "override the org name taken from the file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if file == "" {
		return fmt.Errorf("missing -file. Example: 2k9s import -file accounts.txt")
	}

	oc, err := ImportFromTextFile(file)
	if err != nil {
		return err
	}
	if orgName != "" {
		oc.Name = sanitizeName(orgName)
	}
	if len(oc.DefaultRegions) == 0 {
		oc.DefaultRegions = []string{oc.SSORegion}
	}

	cfg := &Config{Version: configVersion}
	if ConfigExists() {
		if c, err := LoadConfig(); err == nil {
			cfg = c
		}
	}
	if old := cfg.FindOrg(oc.Name); old != nil {
		merged, added, _ := mergeAccounts(old.Accounts, oc.Accounts)
		oc.Accounts = merged
		if oc.LoginProfile == "" {
			oc.LoginProfile = old.LoginProfile
		}
		infof("org %s already exists, added %d new accounts", oc.Name, len(added))
	}
	sort.Slice(oc.Accounts, func(i, j int) bool { return oc.Accounts[i].Name < oc.Accounts[j].Name })

	cfg.UpsertOrg(oc)
	if err := SaveConfig(cfg); err != nil {
		return err
	}
	okf("imported %d accounts into org %s -> %s", len(oc.Accounts), oc.Name, ConfigPath())
	return nil
}

// ---------------------------------------------------------- accounts, config

func cmdAccounts(args []string) error {
	var orgFilter string
	fs := newFlagSet("accounts", "print the account list from config", "")
	fs.StringVar(&orgFilter, "org", "", "only show one org")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	accts, err := cfg.Runtime(orgFilter)
	if err != nil {
		return err
	}
	printAccounts(accts)
	return nil
}

func cmdConfig(args []string) error {
	sub := "path"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "-h", "--help", "help":
		fmt.Fprint(os.Stderr, `2k9s config — view/edit the config file

Usage:
  2k9s config path      print the config file path
  2k9s config show      print the config contents
  2k9s config edit      open the config with $EDITOR (default vi)

Config: ~/.config/2k9s/config.json   (override with the TWOK9S_CONFIG variable)
`)
		return nil
	case "path":
		fmt.Println(ConfigPath())
		return nil
	case "show":
		b, err := os.ReadFile(ConfigPath())
		if err != nil {
			return err
		}
		fmt.Print(string(b))
		return nil
	case "edit":
		ed := os.Getenv("EDITOR")
		if ed == "" {
			ed = "vi"
		}
		return runInteractive(ed, ConfigPath())
	default:
		return fmt.Errorf("2k9s config accepts: path | show | edit")
	}
}
