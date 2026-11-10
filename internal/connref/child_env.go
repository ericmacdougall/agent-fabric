package connref

import "strings"

// ChildEnvironment removes inherited database source configuration before a
// caller appends its explicitly resolved child DSN. It is not a general secret
// or environment sanitizer: production launchers still use their allow-lists.
func ChildEnvironment(base []string) []string {
	prefixes := map[string]bool{"FABRIC_DSN": true, "FABRIC_RUNNER_DSN": true, "FABRIC_TEST_DSN": true, "FABRIC_REVIEW_DSN": true, "FABRIC_STORETEST_CHILD_DSN": true, "FABRIC_HELPER_DSN": true}
	for _, entry := range base {
		name, value, _ := strings.Cut(entry, "=")
		if name == "FABRIC_RUNNER_DSN_ENV" && configName.MatchString(value) {
			prefixes[value] = true
		}
	}
	out := make([]string, 0, len(base))
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if name == "FABRIC_RUNNER_DSN_ENV" || prefixes[name] {
			continue
		}
		drop := false
		for _, suffix := range []string{"_FILE", "_PASSWORD_FILE", "_FILE_KEY", "_DATABASE"} {
			if strings.HasSuffix(name, suffix) && prefixes[strings.TrimSuffix(name, suffix)] {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, entry)
		}
	}
	return out
}
