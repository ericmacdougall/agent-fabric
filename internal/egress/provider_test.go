package egress

import (
	"reflect"
	"testing"
)

func TestCompiledProviderDestinations(t *testing.T) {
	for _, tc := range []struct {
		p     Provider
		extra []string
		want  []string
	}{
		{"", nil, []string{"api.anthropic.com:443"}},
		{ProviderClaude, []string{"claude.ai:443"}, []string{"api.anthropic.com:443", "claude.ai:443"}},
		{ProviderCodex, nil, []string{"chatgpt.com:443"}},
	} {
		got, err := providerDestinations(tc.p, tc.extra)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("provider %q: %v %v", tc.p, got, err)
		}
	}
	for _, tc := range []struct {
		p     Provider
		extra []string
	}{
		{"other", nil}, {ProviderCodex, []string{"claude.ai:443"}},
		{ProviderCodex, []string{"chatgpt.com:443"}}, {ProviderClaude, []string{"example.com:443"}},
		{ProviderClaude, []string{"api.anthropic.com:80"}},
	} {
		if _, err := providerDestinations(tc.p, tc.extra); err == nil {
			t.Fatalf("accepted %q %v", tc.p, tc.extra)
		}
	}
}
