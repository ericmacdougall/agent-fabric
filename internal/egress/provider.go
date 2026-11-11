package egress

import "errors"

// Provider selects a closed, compiled destination policy. The empty value
// preserves the existing Claude policy; it never enables a second provider.
type Provider string

const (
	ProviderClaude Provider = "claude"
	ProviderCodex  Provider = "codex"
)

func providerDestinations(p Provider, extra []string) ([]string, error) {
	if p == ProviderCodex {
		if len(extra) != 0 {
			return nil, errors.New("egress: Codex policy rejects additional destinations")
		}
		return []string{"chatgpt.com:443"}, nil
	}
	if p != "" && p != ProviderClaude {
		return nil, errors.New("egress: unknown provider policy")
	}
	names := []string{DefaultDestination}
	seen := map[string]bool{DefaultDestination: true}
	for _, name := range extra {
		key, _, _, err := parseDestination(name)
		if err != nil || !officialLoginDestinations[key] {
			return nil, errors.New("egress: unsupported Claude login destination")
		}
		if !seen[key] {
			seen[key] = true
			names = append(names, key)
		}
	}
	return names, nil
}
