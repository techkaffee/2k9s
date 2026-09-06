package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Caches the EKS cluster list so we don't have to call the API again next time.

const cacheTTL = 24 * time.Hour

type cacheEntry struct {
	Clusters  []string  `json:"clusters"`
	FetchedAt time.Time `json:"fetched_at"`
}

type clusterCache struct {
	path    string
	Entries map[string]cacheEntry `json:"entries"`
}

func cachePath() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".cache", "2k9s", "clusters.json")
	}
	return filepath.Join(os.TempDir(), "2k9s-clusters.json")
}

func loadCache() *clusterCache {
	c := &clusterCache{path: cachePath(), Entries: map[string]cacheEntry{}}
	b, err := os.ReadFile(c.path)
	if err != nil {
		return c
	}
	var on struct {
		Entries map[string]cacheEntry `json:"entries"`
	}
	if json.Unmarshal(b, &on) == nil && on.Entries != nil {
		c.Entries = on.Entries
	}
	return c
}

func (c *clusterCache) get(accountID, region string) ([]string, bool) {
	e, ok := c.Entries[accountID+"/"+region]
	if !ok || time.Since(e.FetchedAt) > cacheTTL {
		return nil, false
	}
	return e.Clusters, true
}

func (c *clusterCache) put(accountID, region string, clusters []string) {
	c.Entries[accountID+"/"+region] = cacheEntry{Clusters: clusters, FetchedAt: time.Now()}
}

func (c *clusterCache) save() {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return
	}
	b, err := json.MarshalIndent(struct {
		Entries map[string]cacheEntry `json:"entries"`
	}{c.Entries}, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(c.path, b, 0o600)
}
