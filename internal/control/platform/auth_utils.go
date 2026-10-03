package platform

import "strings"

func sanitizeForID(v string) string {
	v = strings.TrimSpace(strings.ToLower(v))
	out := make([]rune, 0, len(v))
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z':
			out = append(out, r)
		case r >= '0' && r <= '9':
			out = append(out, r)
		case r == '_' || r == '-':
			out = append(out, r)
		case r == ' ' || r == '\t' || r == '\n':
			if len(out) > 0 && out[len(out)-1] != '-' {
				out = append(out, '-')
			}
		}
	}
	id := strings.Trim(strings.TrimSpace(string(out)), "-")
	if id == "" {
		return "user"
	}
	return id
}
