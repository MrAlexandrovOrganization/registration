package service

import "strings"

// startSource recognizes Telegram deep links. Malformed payloads still open the
// survey, but are counted as untagged rather than stored as arbitrary text.
func startSource(text string) (source string, start bool) {
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return "", false
	}
	command, suffix, addressed := strings.Cut(parts[0], "@")
	if command != "/start" || (addressed && suffix == "") {
		return "", false
	}
	if len(parts) != 2 || len(parts[1]) > 64 {
		return "", true
	}
	for _, c := range parts[1] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return "", true
		}
	}
	return parts[1], true
}
