package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Step 0 of every run: make sure the required CLIs are present.
// If missing, offer to install via Homebrew (asking for confirmation unless -y).

type tool struct {
	Bin      string // binary name to look up in PATH
	Formula  string // Homebrew formula name
	Required bool
	Why      string
}

var allTools = []tool{
	{Bin: "aws", Formula: "awscli", Required: true, Why: "calls the AWS SSO + EKS APIs"},
	{Bin: "kubectl", Formula: "kubernetes-cli", Required: true, Why: "reads/writes the kubeconfig context"},
	{Bin: "k9s", Formula: "k9s", Required: true, Why: "the cluster UI — this tool's whole point"},
	{Bin: "fzf", Formula: "fzf", Required: false, Why: "type-to-filter picker; falls back to a numbered list without it"},
}

// Common Homebrew directories. Needed because a just-installed binary might
// not yet be in the running process's PATH.
var brewDirs = []string{
	"/opt/homebrew/bin",              // macOS arm64
	"/usr/local/bin",                 // macOS intel
	"/home/linuxbrew/.linuxbrew/bin", // linuxbrew
}

func pathHas(dir string) bool {
	for _, p := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if p == dir {
			return true
		}
	}
	return false
}

// EnsureBrewInPath appends (never prepends, so it won't shadow a binary the
// user chose deliberately) the brew directories to the process's PATH. This
// lets a tool that was just installed be used right away without opening a
// new shell, and this PATH also carries over to k9s via syscall.Exec.
func EnsureBrewInPath() {
	for _, d := range brewDirs {
		if pathHas(d) {
			continue
		}
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			os.Setenv("PATH", os.Getenv("PATH")+string(os.PathListSeparator)+d)
		}
	}
}

func have(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

// neededTools: -l only reads a file so nothing is needed; -c doesn't call AWS so aws isn't needed.
func neededTools(o options) []tool {
	if o.listOnly {
		return nil
	}
	var out []tool
	for _, t := range allTools {
		if o.contextMode && t.Bin == "aws" {
			continue
		}
		out = append(out, t)
	}
	return out
}

func awsVersion() (isV2 bool, version string) {
	out, err := exec.Command("aws", "--version").CombinedOutput()
	if err != nil {
		return false, ""
	}
	v := strings.TrimSpace(string(out))
	if i := strings.IndexAny(v, " \n"); i > 0 {
		v = v[:i]
	}
	return strings.HasPrefix(v, "aws-cli/2"), v
}

func bins(ts []tool) string {
	var s []string
	for _, t := range ts {
		s = append(s, t.Bin)
	}
	return strings.Join(s, ", ")
}

func formulas(ts []tool) []string {
	seen := map[string]bool{}
	var s []string
	for _, t := range ts {
		if seen[t.Formula] {
			continue
		}
		seen[t.Formula] = true
		s = append(s, t.Formula)
	}
	return s
}

// checkTools classifies missing tools. verbose=true prints the status of each one.
func checkTools(o options, verbose bool) (missingRequired, missingOptional []tool) {
	for _, t := range neededTools(o) {
		if have(t.Bin) {
			if verbose {
				p, _ := exec.LookPath(t.Bin)
				okf("%-8s %s", t.Bin, p)
			}
			continue
		}
		if t.Required {
			missingRequired = append(missingRequired, t)
		} else {
			missingOptional = append(missingOptional, t)
		}
		if verbose {
			tag := "optional"
			if t.Required {
				tag = "REQUIRED"
			}
			warnf("%-8s missing [%s] — %s", t.Bin, tag, t.Why)
		}
	}
	return
}

// EnsureTools is step 0. If nothing is missing, it returns immediately without printing anything (fast path).
func EnsureTools(o options) error {
	EnsureBrewInPath()

	missReq, missOpt := checkTools(o, o.checkOnly)

	if o.checkOnly {
		if have("aws") {
			if v2, v := awsVersion(); v != "" && !v2 {
				warnf("aws cli should be v2 (SSO needs v2), currently %s", v)
			}
		}
		switch {
		case len(missReq) > 0:
			return fmt.Errorf("missing required tools: %s — install with: brew install %s",
				bins(missReq), strings.Join(formulas(append(missReq, missOpt...)), " "))
		case len(missOpt) > 0:
			infof("enough tools to run; missing optional tools: %s", bins(missOpt))
		default:
			okf("all tools present, nothing missing")
		}
		return nil
	}

	if len(missReq) == 0 {
		return nil
	}

	// Install optional tools too so we don't have to do this twice.
	toInstall := append(append([]tool{}, missReq...), missOpt...)
	cmdStr := "brew install " + strings.Join(formulas(toInstall), " ")

	warnf("first run: missing required tools")
	for _, t := range append(append([]tool{}, missReq...), missOpt...) {
		tag := "required"
		if !t.Required {
			tag = "optional"
		}
		fmt.Fprintf(os.Stderr, "         - %-8s [%s] %s\n", t.Bin, tag, t.Why)
	}

	if o.noInstall {
		return fmt.Errorf("missing %s. Install manually with: %s", bins(missReq), cmdStr)
	}

	brew, err := exec.LookPath("brew")
	if err != nil {
		return fmt.Errorf("missing %s and this machine has no Homebrew to auto-install with.\n"+
			"       Install Homebrew from https://brew.sh then run: %s", bins(missReq), cmdStr)
	}

	if !o.assumeYes {
		// Don't auto-install when stdin isn't a terminal (cron/CI) — too surprising.
		if !isTTY(os.Stdin) {
			return fmt.Errorf("need to install %s but stdin isn't a terminal.\n"+
				"       Re-run with -y, or run manually: %s", bins(missReq), cmdStr)
		}
		fmt.Fprintf(os.Stderr, "\nWill run: %s%s%s\nContinue? [y/N]: ", cBold, cmdStr, cNC)
		sc := bufio.NewScanner(os.Stdin)
		if !sc.Scan() {
			return errNoSelection
		}
		switch strings.ToLower(strings.TrimSpace(sc.Text())) {
		case "y", "yes":
		default:
			return fmt.Errorf("cancelled. Install manually with: %s", cmdStr)
		}
	}

	infof("%s", cmdStr)
	c := exec.Command(brew, append([]string{"install"}, formulas(toInstall)...)...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stderr, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("brew install failed: %w\n       Try running it manually: %s", err, cmdStr)
	}

	EnsureBrewInPath()
	if stillMissing, _ := checkTools(o, false); len(stillMissing) > 0 {
		return fmt.Errorf("installed but still can't find %s in PATH — open a new shell and try again",
			bins(stillMissing))
	}
	if v2, v := awsVersion(); v != "" && !v2 {
		warnf("aws cli is %s, SSO needs v2 — consider: brew install awscli", v)
	}
	okf("all tools installed")
	return nil
}
