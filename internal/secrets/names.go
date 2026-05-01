package secrets

import "strings"

func IsSensitiveName(name string) bool {
	lower := strings.ToLower(name)
	for _, token := range []string{"password", "passwd", "secret", "token", "key", "credential"} {
		if strings.Contains(lower, token) {
			return true
		}
	}
	return false
}
