// 2k9s — pick an AWS account from a list, then open k9s straight into that
// account's EKS cluster.
//
// Default flow:
//  0. Check aws/kubectl/k9s (+ optional fzf); if missing, offer to install via brew
//  1. Read accounts*.txt (shared format with acm-audit)
//  2. Show the account list to pick from (fzf if available, otherwise a numbered list)
//  3. Get an SSO token, auto-discover an available role in the account
//  4. List EKS clusters in the org's default regions -> pick a cluster
//  5. Ensure a profile exists in ~/.aws/config, run aws eks update-kubeconfig
//  6. exec k9s into the right context
//
// Quick mode: `2k9s -c` picks directly from a context already in kubeconfig,
// without calling AWS.
//
// Note: the module in go.mod is named `twok9s` because the binary name starts
// with a digit; the actual binary name is decided by `go build -o 2k9s`.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
)

var (
	colorOff = os.Getenv("NO_COLOR") != "" || !isTTY(os.Stderr)

	cRed    = color("\033[0;31m")
	cGreen  = color("\033[0;32m")
	cYellow = color("\033[1;33m")
	cBlue   = color("\033[0;34m")
	cDim    = color("\033[2m")
	cBold   = color("\033[1m")
	cNC     = color("\033[0m")
)

func color(s string) string {
	if colorOff {
		return ""
	}
	return s
}

func isTTY(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

func infof(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%s[2k9s]%s  %s\n", cBlue, cNC, fmt.Sprintf(format, a...))
}
func warnf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%s[warn]%s  %s\n", cYellow, cNC, fmt.Sprintf(format, a...))
}
func okf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%s[ok]%s    %s\n", cGreen, cNC, fmt.Sprintf(format, a...))
}
func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%s[error]%s %s\n", cRed, cNC, fmt.Sprintf(format, a...))
	os.Exit(1)
}

type options struct {
	org         string
	regions     string
	addRegions  string
	namespace   string
	account     string
	cluster     string
	contextMode bool
	listOnly    bool
	refresh     bool
	dryRun      bool
	noLogin     bool
	noK9s       bool
	writeProf   bool
	checkOnly   bool
	assumeYes   bool
	noInstall   bool
	showVersion bool
}

// newFlagSet is shared by all subcommands. desc/extra are shown when --help is called.
func newFlagSet(name, desc, extra string) *flag.FlagSet {
	fs := flag.NewFlagSet("2k9s "+name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "2k9s %s — %s\n", name, desc)
		if extra != "" {
			fmt.Fprintf(os.Stderr, "\n%s\n", extra)
		}
		fmt.Fprint(os.Stderr, "\nFlags:\n")
		fs.PrintDefaults()
	}
	return fs
}

// runInteractive runs a command with the user's TTY (used by `config edit`).
func runInteractive(bin string, args ...string) error {
	c := exec.Command(bin, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

var subcommands = map[string]func([]string) error{
	"init":     cmdInit,
	"sync":     cmdSync,
	"import":   cmdImport,
	"accounts": cmdAccounts,
	"config":   cmdConfig,
	"help":     cmdHelp,
	"version":  cmdVersion,
}

// cmdHelp is for `2k9s help`. Must not reference the `subcommands` variable
// here, otherwise Go reports an initialization cycle (subcommands -> cmdHelp
// -> subcommands).
func cmdHelp([]string) error {
	var o options
	printRootUsage(buildRootFlags(&o))
	return nil
}

func main() {
	// Subcommand: first argument doesn't start with '-'
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		name := os.Args[1]
		fn, ok := subcommands[name]
		if !ok {
			fatalf("invalid command: %q\n"+
				"        available commands: init, sync, import, accounts, config, help\n"+
				"        run `2k9s help` for usage", name)
		}
		if err := fn(os.Args[2:]); err != nil {
			// `2k9s <cmd> --help` -> flag already printed usage, exit 0 rather than treat as an error
			if errors.Is(err, flag.ErrHelp) {
				os.Exit(0)
			}
			if errors.Is(err, errNoSelection) {
				infof("cancelled")
				os.Exit(130)
			}
			fatalf("%v", err)
		}
		return
	}

	var o options
	fs := buildRootFlags(&o)

	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(2)
	}

	if err := run(o); err != nil {
		if errors.Is(err, errNoSelection) {
			infof("cancelled")
			os.Exit(130)
		}
		fatalf("%v", err)
	}
}

// buildRootFlags declares the root command's flags. Split into its own
// function so `2k9s help` prints the same Flags section as `2k9s --help`.
func buildRootFlags(o *options) *flag.FlagSet {
	fs := flag.NewFlagSet("2k9s", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&o.org, "org", "", "only include accounts from one org (org name in config)")
	fs.StringVar(&o.regions, "region", "", "override the region(s) to search for clusters (comma-separated)")
	fs.StringVar(&o.addRegions, "add-region", "", "also search this region in addition to the org's default")
	fs.StringVar(&o.namespace, "n", "", "namespace passed to k9s")
	fs.StringVar(&o.account, "a", "", "pick an account directly by name, skipping the list step")
	fs.StringVar(&o.cluster, "cluster", "", "pick a cluster directly by name, skipping the list step")
	fs.BoolVar(&o.contextMode, "c", false, "pick directly from an existing kubeconfig context (no AWS calls)")
	fs.BoolVar(&o.listOnly, "l", false, "print the account list and exit")
	fs.BoolVar(&o.refresh, "refresh", false, "skip the cache, call eks list-clusters again")
	fs.BoolVar(&o.dryRun, "dry-run", false, "print what would happen, without running k9s")
	fs.BoolVar(&o.noLogin, "no-login", false, "don't auto-run aws sso login when the token expires")
	fs.BoolVar(&o.noK9s, "no-k9s", false, "only set kubeconfig, don't open k9s")
	fs.BoolVar(&o.writeProf, "write-profile", true, "create a profile in ~/.aws/config if missing")
	fs.BoolVar(&o.checkOnly, "check", false, "check required tools (aws/kubectl/k9s/fzf) and exit")
	fs.BoolVar(&o.assumeYes, "y", false, "auto-confirm installing missing tools, don't prompt")
	fs.BoolVar(&o.noInstall, "no-install", false, "don't auto-install tools, just error if missing")
	fs.BoolVar(&o.showVersion, "version", false, "print the 2k9s version and exit")

	fs.Usage = func() { printRootUsage(fs) }
	return fs
}

func printRootUsage(fs *flag.FlagSet) {
	fmt.Fprint(os.Stderr, `2k9s — pick an AWS account from a list, then open k9s into that account's EKS cluster

Usage:
  2k9s [flags]
  2k9s <command> [flags]

Commands:
  init                      import accounts from AWS SSO into config (run first)
  sync                      refresh the account list from AWS SSO
  import -file f.txt        import accounts from a flat file
  accounts                  print the account list from config
  config path|show|edit     view/edit the config file
  help                      print this help
  version                   print the 2k9s version (same as --version)

Examples:
  2k9s                      pick account -> pick cluster -> open k9s
  2k9s -c                   quick-pick from an existing kubeconfig context
  2k9s --org acme           only show accounts from one org
  2k9s -n kube-system       open k9s in the given namespace
  2k9s -a prod -cluster k1  pick directly, skip showing the list
  2k9s -l                   print the account list
  2k9s --refresh            refresh the cluster list cache
  2k9s --dry-run            show what would happen without writing anything
  2k9s --check              check whether aws/kubectl/k9s/fzf are present

See help for each command:
  2k9s init --help
  2k9s sync --help

First run: if aws/kubectl/k9s are missing, 2k9s offers to install them via Homebrew,
then automatically runs "init" to import accounts.
Config: ~/.config/2k9s/config.json   (override with the TWOK9S_CONFIG variable)

Flags:
`)
	fs.PrintDefaults()
}

func run(o options) error {
	if o.showVersion {
		return cmdVersion(nil)
	}
	// Step 0: ensure aws/kubectl/k9s (+ optional fzf) are present, install via brew if missing.
	if err := EnsureTools(o); err != nil {
		return err
	}
	if o.checkOnly {
		return nil
	}

	if o.contextMode {
		return runContextMode(o)
	}
	return runAccountMode(o)
}

// ------------------------------------------------------- existing-context mode

func runContextMode(o options) error {
	ctxs, err := ListKubeContexts()
	if err != nil {
		return err
	}
	if len(ctxs) == 0 {
		return fmt.Errorf("kubeconfig has no contexts yet, run `2k9s` (without -c) to create one")
	}

	// map accountID -> account name for readable display (use config if present, otherwise skip)
	names := map[string]string{}
	if cfg, err := LoadConfig(); err == nil {
		if accts, err := cfg.Runtime(o.org); err == nil {
			for _, a := range accts {
				names[a.ID] = a.Name
			}
		}
	}

	sort.Slice(ctxs, func(i, j int) bool { return ctxs[i].Name < ctxs[j].Name })

	items := make([]Item, 0, len(ctxs))
	for _, c := range ctxs {
		mark := " "
		if c.Current {
			mark = "*"
		}
		acct := names[c.AccountID]
		if acct == "" {
			acct = c.AccountID
		}
		region := c.Region
		if region == "" {
			region = "-"
		}
		items = append(items, Item{
			Key:  c.Name,
			Cols: []string{mark, c.Name, acct, region},
		})
	}

	sel, err := Pick("context", "pick a context (* = currently in use)", items)
	if err != nil {
		return err
	}

	var region string
	for _, c := range ctxs {
		if c.Name == sel {
			region = c.Region
		}
	}
	okf("context: %s", sel)
	return launchK9s(sel, o, region, "")
}

// --------------------------------------------------------- account-picking mode

func runAccountMode(o options) error {
	// First run: no config yet -> run init to import accounts from SSO.
	if !ConfigExists() {
		infof("no config yet (%s) — running first-time import", ConfigPath())
		if err := cmdInit(nil); err != nil {
			return err
		}
	}

	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	accts, err := cfg.Runtime(o.org)
	if err != nil {
		return err
	}
	infof("%d accounts from %s", len(accts), ConfigPath())

	if o.listOnly {
		printAccounts(accts)
		return nil
	}

	// 1) pick an account
	var acct Account
	if o.account != "" {
		for _, a := range accts {
			if a.Name == o.account {
				acct = a
				break
			}
		}
		if acct.Name == "" {
			return fmt.Errorf("no account named %q (use -l to see the list)", o.account)
		}
	} else {
		items := make([]Item, 0, len(accts))
		for _, a := range accts {
			items = append(items, Item{
				Key:  a.Org.Label + "/" + a.Name,
				Cols: []string{a.Name, a.Org.Label, a.ID, strings.Join(regionsFor(a, o), ",")},
			})
		}
		selKey, err := Pick("account", "pick an AWS account", items)
		if err != nil {
			return err
		}
		for _, a := range accts {
			if a.Org.Label+"/"+a.Name == selKey {
				acct = a
				break
			}
		}
		if acct.Name == "" {
			return fmt.Errorf("couldn't map selection %q", selKey)
		}
	}
	regions := regionsFor(acct, o)
	okf("account: %s (%s) — org %s", acct.Name, acct.ID, acct.Org.Label)

	// 2) SSO token + role
	token, err := EnsureSSOToken(acct.Org, !o.noLogin)
	if err != nil {
		return err
	}
	roles, err := ListAccountRoles(token, acct.ID, acct.Org.SSORegion)
	if isSSOUnauthorized(err) {
		// cached token looked valid but AWS rejected it -> force relogin, retry once.
		token, err = ForceSSOLogin(acct.Org, !o.noLogin)
		if err == nil {
			roles, err = ListAccountRoles(token, acct.ID, acct.Org.SSORegion)
		}
	}
	if err != nil {
		return fmt.Errorf("couldn't list roles for account %s: %w", acct.Name, err)
	}
	role := PickRole(roles, acct.Org.RolePreference)
	if role == "" {
		return fmt.Errorf("you have no SSO role in account %s (%s)", acct.Name, acct.ID)
	}
	infof("role: %s", role)

	creds, err := GetRoleCredentials(token, acct.ID, role, acct.Org.SSORegion)
	if isSSOUnauthorized(err) {
		token, err = ForceSSOLogin(acct.Org, !o.noLogin)
		if err == nil {
			creds, err = GetRoleCredentials(token, acct.ID, role, acct.Org.SSORegion)
		}
	}
	if err != nil {
		return err
	}

	// 3) find clusters across the regions
	found, err := findClusters(acct, creds, regions, o.refresh)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		return fmt.Errorf("no EKS clusters found in account %s in region(s): %s\n"+
			"       try: 2k9s --add-region <region>  or  --region <region>",
			acct.Name, strings.Join(regions, ", "))
	}

	var csel string
	if o.cluster != "" {
		for _, fc := range found {
			if fc.Name == o.cluster {
				csel = fc.Region + "/" + fc.Name
				break
			}
		}
		if csel == "" {
			var have []string
			for _, fc := range found {
				have = append(have, fc.Name)
			}
			return fmt.Errorf("account %s has no cluster %q (available: %s)",
				acct.Name, o.cluster, strings.Join(have, ", "))
		}
	} else {
		citems := make([]Item, 0, len(found))
		for _, fc := range found {
			citems = append(citems, Item{
				Key:  fc.Region + "/" + fc.Name,
				Cols: []string{fc.Name, fc.Region},
			})
		}
		csel, err = Pick("cluster", fmt.Sprintf("pick an EKS cluster in %s", acct.Name), citems)
		if err != nil {
			return err
		}
	}
	slash := strings.Index(csel, "/")
	region, cluster := csel[:slash], csel[slash+1:]
	okf("cluster: %s @ %s", cluster, region)

	// 4) profile + kubeconfig
	profile := acct.Name
	if o.dryRun {
		fmt.Printf("DRY RUN\n")
		fmt.Printf("  account        : %s (%s)\n", acct.Name, acct.ID)
		fmt.Printf("  org            : %s\n", acct.Org.Label)
		fmt.Printf("  role           : %s\n", role)
		fmt.Printf("  cluster        : %s\n", cluster)
		fmt.Printf("  region         : %s\n", region)
		fmt.Printf("  aws profile    : %s (exists: %v)\n", profile, ProfileExists(profile))
		fmt.Printf("  will run       : aws eks update-kubeconfig --region %s --name %s --alias %s --profile %s\n",
			region, cluster, cluster, profile)
		fmt.Printf("  then           : k9s --context %s%s\n", cluster, nsSuffix(o.namespace))
		return nil
	}

	if o.writeProf {
		created, err := EnsureProfile(acct, role, region)
		if err != nil {
			return fmt.Errorf("couldn't create profile %s: %w", profile, err)
		}
		if created {
			okf("added [profile %s] to ~/.aws/config", profile)
		}
	} else if !ProfileExists(profile) {
		return fmt.Errorf("profile %s doesn't exist in ~/.aws/config (remove -write-profile=false to auto-create it)", profile)
	}

	infof("aws eks update-kubeconfig --name %s --region %s --alias %s", cluster, region, cluster)
	if err := UpdateKubeconfig(profile, region, cluster, cluster); err != nil {
		return err
	}
	okf("kubeconfig -> context %s", cluster)

	return launchK9s(cluster, o, region, profile)
}

// ------------------------------------------------------------------- helpers

type foundCluster struct {
	Name   string
	Region string
}

// regionsFor: the org's default regions, overridden by --region, extended by --add-region.
func regionsFor(a Account, o options) []string {
	regions := a.Org.DefaultRegions
	if o.regions != "" {
		regions = splitCSV(o.regions)
	}
	if o.addRegions != "" {
		regions = splitCSV(strings.Join(regions, ",") + "," + o.addRegions)
	}
	if len(regions) == 0 {
		regions = []string{"ap-southeast-1"}
	}
	return regions
}

// findClusters scans regions in parallel, with a 24h cache.
func findClusters(acct Account, creds Creds, regions []string, refresh bool) ([]foundCluster, error) {
	cache := loadCache()

	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		found   []foundCluster
		errs    []string
		changed bool
	)

	for _, r := range regions {
		region := r
		if !refresh {
			if cl, ok := cache.get(acct.ID, region); ok {
				for _, c := range cl {
					found = append(found, foundCluster{Name: c, Region: region})
				}
				continue
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			cl, err := ListClusters(envWithCreds(creds, region), region)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", region, err))
				return
			}
			cache.put(acct.ID, region, cl)
			changed = true
			for _, c := range cl {
				found = append(found, foundCluster{Name: c, Region: region})
			}
		}()
	}
	wg.Wait()

	if changed {
		cache.save()
	}
	for _, e := range errs {
		warnf("list-clusters %s", e)
	}
	if len(found) == 0 && len(errs) == len(regions) && len(errs) > 0 {
		return nil, fmt.Errorf("couldn't call eks list-clusters in any region")
	}

	sort.Slice(found, func(i, j int) bool {
		if found[i].Region != found[j].Region {
			return found[i].Region < found[j].Region
		}
		return found[i].Name < found[j].Name
	})
	return found, nil
}

func nsSuffix(ns string) string {
	if ns == "" {
		return ""
	}
	return " -n " + ns
}

// launchK9s replaces the current process with k9s so it takes full control of the TTY.
func launchK9s(context string, o options, region, profile string) error {
	if o.noK9s {
		okf("skipping k9s (-no-k9s). Use: kubectl --context %s get pods", context)
		return nil
	}

	bin, err := exec.LookPath("k9s")
	if err != nil {
		return fmt.Errorf("k9s not found in PATH: %w", err)
	}

	args := []string{"k9s", "--context", context}
	if o.namespace != "" {
		args = append(args, "-n", o.namespace)
	}

	env := baseEnv()
	if profile != "" {
		env = append(env, "AWS_PROFILE="+profile)
	}
	if region != "" {
		env = append(env, "AWS_REGION="+region, "AWS_DEFAULT_REGION="+region)
	}

	infof("k9s --context %s%s", context, nsSuffix(o.namespace))
	return syscall.Exec(bin, args, env)
}

func printAccounts(accts []Account) {
	items := make([]Item, 0, len(accts))
	for _, a := range accts {
		items = append(items, Item{Cols: []string{a.Name, a.Org.Label, a.ID, a.Email}})
	}
	for _, line := range alignCols(items) {
		fmt.Println(line)
	}
	fmt.Printf("%s(%d accounts)%s\n", cDim, len(accts), cNC)
}
