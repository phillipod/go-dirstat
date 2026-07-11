// Package fileclass centralizes filesystem-name classifications that must stay
// identical across analytical and machine-readable surfaces.
package fileclass

import "strings"

// Extension returns a lower-case final extension with its leading dot. A
// leading-only dot does not make a Unix dotfile an extension: .env and
// .gitignore therefore return an empty string, while .config.json returns
// .json.
func Extension(name string) string {
	index := strings.LastIndexByte(name, '.')
	if index <= 0 || index == len(name)-1 {
		return ""
	}
	return strings.ToLower(name[index:])
}
