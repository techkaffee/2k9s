package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Runtime types used throughout the tool. The data source is config.json
// (see config.go); the .txt file is just one import path.

type Org struct {
	Label          string
	StartURL       string
	SSORegion      string
	DefaultRegions []string
	LoginProfile   string
	RolePreference []string
}

type Account struct {
	Name  string
	ID    string
	Email string
	Org   *Org
}

// splitCSV: split on commas, trim whitespace, drop empties, dedupe (keeping order).
func splitCSV(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// ImportFromTextFile reads a flat file of the form:
//
//	# org: <name>
//	# sso-start-url: https://...
//	# sso-region: ap-southeast-1
//	# default-regions: ap-southeast-1,us-east-1
//	# login-profile: <profile>          (optional)
//	<account-name>,<12 digits>,<optional email>
//
// Blank lines and '#' lines that aren't directives are skipped.
// This is a secondary import path; the main source is `2k9s init`, which
// pulls directly from AWS SSO.
func ImportFromTextFile(path string) (*OrgConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	oc := &OrgConfig{}
	var lineNo int

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(strings.TrimRight(sc.Text(), "\r"))
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "#") {
			body := strings.TrimSpace(strings.TrimLeft(line, "#"))
			i := strings.Index(body, ":")
			if i <= 0 {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(body[:i]))
			val := strings.TrimSpace(body[i+1:])
			if val == "" {
				continue
			}
			switch key {
			case "org":
				oc.Name = sanitizeName(val)
			case "sso-start-url":
				oc.SSOStartURL = val
			case "sso-region":
				oc.SSORegion = val
			case "login-profile":
				oc.LoginProfile = val
			case "default-regions":
				oc.DefaultRegions = splitCSV(val)
			case "role-preference":
				oc.RolePreference = splitCSV(val)
			}
			continue
		}

		parts := strings.Split(line, ",")
		if len(parts) < 2 {
			return nil, fmt.Errorf("%s line %d: need at least 'name,id', got %q",
				filepath.Base(path), lineNo, line)
		}
		name := sanitizeName(parts[0])
		id := strings.TrimSpace(parts[1])
		email := ""
		if len(parts) > 2 {
			email = strings.TrimSpace(parts[2])
		}
		if !accountIDRe.MatchString(id) {
			return nil, fmt.Errorf("%s line %d: account id %q must be exactly 12 digits",
				filepath.Base(path), lineNo, id)
		}
		oc.Accounts = append(oc.Accounts, AccountEntry{Name: name, ID: id, Email: email})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	if oc.Name == "" {
		oc.Name = sanitizeName(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	}
	if len(oc.Accounts) == 0 {
		return nil, fmt.Errorf("%s: no account lines found", filepath.Base(path))
	}
	return oc, nil
}

// mergeAccounts merges the new list into the old one by account ID.
// Keeps names the user has edited by hand, only adding new accounts.
func mergeAccounts(old, fresh []AccountEntry) (merged []AccountEntry, added, removed []AccountEntry) {
	byID := map[string]AccountEntry{}
	for _, a := range old {
		byID[a.ID] = a
	}
	freshIDs := map[string]bool{}

	for _, f := range fresh {
		freshIDs[f.ID] = true
		if cur, ok := byID[f.ID]; ok {
			// respect the name the user set, only fill email if it was previously empty
			if cur.Email == "" {
				cur.Email = f.Email
			}
			merged = append(merged, cur)
			continue
		}
		merged = append(merged, f)
		added = append(added, f)
	}
	for _, a := range old {
		if !freshIDs[a.ID] {
			removed = append(removed, a)
		}
	}
	return merged, added, removed
}
