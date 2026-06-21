package main

import (
	"testing"

	"github.com/charliek/shed/internal/config"
)

func TestEgressProfileSummary(t *testing.T) {
	cases := []struct {
		name string
		p    config.EgressProfile
		want string
	}{
		{"empty", config.EgressProfile{}, "(empty)"},
		{"audit", config.EgressProfile{Mode: "audit"}, "mode=audit"},
		{"allow+deny", config.EgressProfile{Allow: []string{"a.com", "b.com"}, Deny: []string{"x.com"}}, "allow=a.com,b.com deny=x.com"},
		{"rule", config.EgressProfile{Rule: "port == 443"}, "rule=port == 443"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := egressProfileSummary(c.p); got != c.want {
				t.Errorf("summary = %q, want %q", got, c.want)
			}
		})
	}
}
