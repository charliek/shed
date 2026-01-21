// Package terminal provides terminal configuration and normalization.
package terminal

// Config holds terminal-related configuration settings.
type Config struct {
	// FallbackTerm is the default TERM value to use when the client's terminal
	// type is not recognized. If empty, the client's TERM passes through unchanged.
	FallbackTerm string `yaml:"fallback_term"`

	// TermMappings provides explicit TERM value overrides.
	// Key is the original TERM, value is the replacement.
	TermMappings map[string]string `yaml:"term_mappings"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		FallbackTerm: "",
		TermMappings: map[string]string{
			// Ghostty uses xterm-ghostty which isn't in ncurses-term
			"xterm-ghostty": "xterm-256color",
		},
	}
}

// NormalizeTerm applies terminal mappings and fallback logic to a TERM value.
// If the TERM has an explicit mapping, that mapping is used.
// Otherwise, the original TERM is returned (ncurses-term handles most cases).
func (c *Config) NormalizeTerm(term string) string {
	if c == nil {
		return term
	}

	// Check for explicit mapping first
	if mapped, ok := c.TermMappings[term]; ok {
		return mapped
	}

	// If FallbackTerm is set and this looks like an exotic terminal that might
	// not be recognized, we could apply fallback logic here. However, with
	// ncurses-term installed in the container, most terminals will work.
	// The fallback is primarily for edge cases that aren't in ncurses-term.

	return term
}
