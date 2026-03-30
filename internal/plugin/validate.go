package plugin

import (
	"fmt"
	"strings"
	"unicode"
)

const (
	maxNamespaceLength = 128
	systemPrefix       = "system:"
)

// ValidateNamespace checks that a namespace string is valid for external use.
func ValidateNamespace(namespace string) error {
	if namespace == "" {
		return fmt.Errorf("namespace must not be empty")
	}
	if len(namespace) > maxNamespaceLength {
		return fmt.Errorf("namespace must be at most %d characters", maxNamespaceLength)
	}
	if strings.HasPrefix(namespace, systemPrefix) {
		return fmt.Errorf("namespace prefix %q is reserved for internal use", systemPrefix)
	}
	for _, r := range namespace {
		if r == ' ' {
			return fmt.Errorf("namespace must not contain spaces")
		}
		if !unicode.IsPrint(r) || r > unicode.MaxASCII {
			return fmt.Errorf("namespace must contain only printable ASCII characters")
		}
	}
	return nil
}

// IsSystemNamespace reports whether the namespace is reserved for internal use.
func IsSystemNamespace(namespace string) bool {
	return strings.HasPrefix(namespace, systemPrefix)
}
