package hop

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

const redactedMarkerPrefix = "[REDACTED:"

type secretPattern struct {
	pattern  *regexp.Regexp
	group    int
	kind     string
	priority int
}

type secretCandidate struct {
	start    int
	end      int
	kind     string
	priority int
}

var promptSecretPatterns = []secretPattern{
	{
		pattern:  regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
		kind:     "private_key",
		priority: 120,
	},
	{
		pattern:  regexp.MustCompile(`(?s)<secret>.*?</secret>`),
		kind:     "explicit_secret",
		priority: 120,
	},
	{
		pattern:  regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}\b`),
		kind:     "api_key",
		priority: 110,
	},
	{
		pattern:  regexp.MustCompile(`\bsk-(?:proj-|svcacct-)?[A-Za-z0-9_-]{20,}\b`),
		kind:     "api_key",
		priority: 110,
	},
	{
		pattern:  regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`),
		kind:     "access_token",
		priority: 110,
	},
	{
		pattern:  regexp.MustCompile(`\b(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}\b`),
		kind:     "api_key",
		priority: 110,
	},
	{
		pattern:  regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{30,}\b`),
		kind:     "api_key",
		priority: 110,
	},
	{
		pattern:  regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{20,}\b`),
		kind:     "access_token",
		priority: 110,
	},
	{
		pattern:  regexp.MustCompile(`\b(?:AKIA|ASIA|AIDA|AROA|AIPA|ANPA|ANVA|AGPA)[A-Z0-9]{16}\b`),
		kind:     "access_key",
		priority: 110,
	},
	{
		pattern:  regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
		kind:     "auth_token",
		priority: 105,
	},
	{
		pattern:  regexp.MustCompile(`(?i)(authorization[ \t]*:[ \t]*(?:bearer|token|basic)[ \t]+)([A-Za-z0-9._~+/=-]{12,})`),
		group:    2,
		kind:     "auth_token",
		priority: 100,
	},
	{
		pattern:  regexp.MustCompile(`(?i)\b(?:postgres(?:ql)?|mysql|mongodb(?:\+srv)?|redis|https?)://([^@\s/]+)@`),
		group:    1,
		kind:     "connection_credential",
		priority: 100,
	},
	{
		pattern: regexp.MustCompile(
			`(?i)\b[a-z0-9_.-]*(?:api[ _-]?key|access[ _-]?key|secret[ _-]?key|access[ _-]?token|auth[ _-]?token|client[ _-]?secret|api[ _-]?secret|password|passwd|credential|token|secret|key)\b[ \t]*(?:=|:|is\b)[ \t]*["']?([^\s"'` + "`" + `]{12,})`,
		),
		group:    1,
		kind:     "credential",
		priority: 80,
	},
}

// RedactPromptSecrets removes high-confidence credentials before a prompt is
// passed to any durable Hop component. It returns only category/count metadata;
// secret values, hashes, and byte positions are deliberately discarded.
func RedactPromptSecrets(prompt string) (string, []PromptRedaction) {
	if prompt == "" {
		return prompt, nil
	}

	var candidates []secretCandidate
	for _, detector := range promptSecretPatterns {
		for _, match := range detector.pattern.FindAllStringSubmatchIndex(prompt, -1) {
			index := detector.group * 2
			if index+1 >= len(match) || match[index] < 0 || match[index+1] <= match[index] {
				continue
			}
			start, end := match[index], match[index+1]
			if detector.kind == "credential" {
				for end > start && strings.ContainsRune(".,;:!?)]}", rune(prompt[end-1])) {
					end--
				}
			}
			if end <= start || strings.HasPrefix(prompt[start:end], redactedMarkerPrefix) {
				continue
			}
			if detector.kind == "credential" && !likelyCredentialValue(prompt[start:end]) {
				continue
			}
			candidates = append(candidates, secretCandidate{
				start:    start,
				end:      end,
				kind:     detector.kind,
				priority: detector.priority,
			})
		}
	}
	if len(candidates) == 0 {
		return prompt, nil
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].priority != candidates[j].priority {
			return candidates[i].priority > candidates[j].priority
		}
		if candidates[i].start != candidates[j].start {
			return candidates[i].start < candidates[j].start
		}
		return candidates[i].end > candidates[j].end
	})
	selected := make([]secretCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		overlaps := false
		for _, existing := range selected {
			if candidate.start < existing.end && existing.start < candidate.end {
				overlaps = true
				break
			}
		}
		if !overlaps {
			selected = append(selected, candidate)
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].start < selected[j].start })

	counts := make(map[string]int)
	var redacted strings.Builder
	cursor := 0
	for _, candidate := range selected {
		redacted.WriteString(prompt[cursor:candidate.start])
		redacted.WriteString(redactedMarkerPrefix)
		redacted.WriteString(candidate.kind)
		redacted.WriteByte(']')
		counts[candidate.kind]++
		cursor = candidate.end
	}
	redacted.WriteString(prompt[cursor:])

	kinds := make([]string, 0, len(counts))
	for kind := range counts {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	findings := make([]PromptRedaction, 0, len(kinds))
	for _, kind := range kinds {
		findings = append(findings, PromptRedaction{Kind: kind, Count: counts[kind]})
	}
	return redacted.String(), findings
}

func redactSecretStrings(values []string) ([]string, []PromptRedaction) {
	redacted := make([]string, len(values))
	counts := make(map[string]int)
	for index, value := range values {
		var findings []PromptRedaction
		redacted[index], findings = RedactPromptSecrets(value)
		for _, finding := range findings {
			counts[finding.Kind] += finding.Count
		}
	}
	kinds := make([]string, 0, len(counts))
	for kind := range counts {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	findings := make([]PromptRedaction, 0, len(kinds))
	for _, kind := range kinds {
		findings = append(findings, PromptRedaction{Kind: kind, Count: counts[kind]})
	}
	return redacted, findings
}

func likelyCredentialValue(value string) bool {
	if len(value) < 12 {
		return false
	}
	classes := 0
	var lower, upper, digit, symbol bool
	counts := make(map[byte]int)
	for index := 0; index < len(value); index++ {
		character := value[index]
		counts[character]++
		switch {
		case character >= 'a' && character <= 'z':
			lower = true
		case character >= 'A' && character <= 'Z':
			upper = true
		case character >= '0' && character <= '9':
			digit = true
		default:
			symbol = true
		}
	}
	for _, present := range []bool{lower, upper, digit, symbol} {
		if present {
			classes++
		}
	}
	if classes < 2 {
		return false
	}
	entropy := 0.0
	length := float64(len(value))
	for _, count := range counts {
		probability := float64(count) / length
		entropy -= probability * math.Log2(probability)
	}
	return entropy >= 3.0
}
