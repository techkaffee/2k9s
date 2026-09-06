package main

import "fmt"

// version/commit/date are overwritten at build time via -ldflags, e.g.:
//
//	go build -ldflags "-X main.version=v1.2.3 -X main.commit=abcdef -X main.date=2026-09-06T12:00:00Z"
//
// The GitHub Actions release workflow (.github/workflows/release.yml) does
// this automatically based on the git tag when a tag matching v*.*.* is
// pushed. A manual build (make build) will show "dev".
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// cmdVersion is for `2k9s version` and `2k9s --version`.
func cmdVersion([]string) error {
	fmt.Printf("2k9s %s (commit %s, built %s)\n", version, commit, date)
	return nil
}
