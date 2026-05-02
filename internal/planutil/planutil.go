package planutil

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var slugPattern = regexp.MustCompile(`[^a-z0-9._-]+`)

func Slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, " ", "-")
	value = slugPattern.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-._")
	return value
}

var stepIDPattern = regexp.MustCompile(`[^a-z0-9._:-]+`)

func NextStepID(used map[string]int, value string) string {
	base := strings.ToLower(strings.TrimSpace(value))
	base = strings.ReplaceAll(base, " ", "-")
	base = stepIDPattern.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-._:")
	if base == "" {
		base = "step"
	}
	used[base]++
	if used[base] == 1 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, used[base])
}

func OptionalDependency(id string) []string {
	if id == "" {
		return nil
	}
	return []string{id}
}

func UniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	for _, value := range values {
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	unique := make([]string, 0, len(seen))
	for value := range seen {
		unique = append(unique, value)
	}
	sort.Strings(unique)
	return unique
}

func Fallback(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
