package health

import (
	"sort"
	"strings"

	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/config"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/protocol"
)

func Discover(roster []protocol.HostAuthFileEntry, cfg config.Config) []protocol.HostAuthFileEntry {
	result := make([]protocol.HostAuthFileEntry, 0, len(roster))
	seen := make(map[string]struct{}, len(roster))
	for _, entry := range roster {
		provider := providerOf(entry)
		if !cfg.ProviderEnabled(provider) || !IsOAuthCredential(entry) {
			continue
		}
		entry.Provider = provider
		entry.AuthIndex = strings.TrimSpace(entry.AuthIndex)
		if entry.AuthIndex == "" {
			continue
		}
		key := AccountKey(provider, entry.AuthIndex)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Provider == result[j].Provider {
			return result[i].AuthIndex < result[j].AuthIndex
		}
		return result[i].Provider < result[j].Provider
	})
	return result
}
