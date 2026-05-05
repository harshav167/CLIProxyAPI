package cursor

import (
	"fmt"
	"strings"
)

// CredentialFileName returns the filename used to persist Cursor credentials.
func CredentialFileName(label, subHash string) string {
	label = strings.TrimSpace(label)
	subHash = strings.TrimSpace(subHash)
	if label != "" {
		return fmt.Sprintf("cursor.%s.json", label)
	}
	if subHash != "" {
		return fmt.Sprintf("cursor.%s.json", subHash)
	}
	return "cursor.json"
}

// DisplayLabel returns a human-readable label for the Cursor account.
func DisplayLabel(label, subHash string) string {
	label = strings.TrimSpace(label)
	if label != "" {
		return "Cursor " + label
	}
	if subHash != "" {
		return "Cursor " + subHash
	}
	return "Cursor User"
}
