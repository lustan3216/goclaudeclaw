// sections.go implements section parsing and relevance scoring for memory.md.
//
// Section format (HTML comment header):
//
//	<!-- section: name tags: tag1,tag2,chinese,english -->
//	## Section Title
//	content...
//
// Sections with the "always" tag are injected every time (suitable for global preferences, etc.).
// Other sections are scored by tag hit count and only relevant ones are injected.
// If the file has no section markers, the whole file is treated as a global/always section (backward-compatible).
package memory

import (
	"regexp"
	"strings"
)

// maxExtraSections controls the maximum number of extra sections injected beyond the "always" ones.
const maxExtraSections = 3

// maxSectionBytes is the byte limit for a single section (prevents one huge section consuming all tokens).
const maxSectionBytes = 1500

var sectionHeaderRe = regexp.MustCompile(
	`<!--\s*section:\s*(\S+)\s+tags:\s*([^-]+?)\s*-->`)

// Section represents an independent memory fragment in memory.md.
type Section struct {
	Name    string
	Tags    []string // includes bilingual synonyms; "always" = inject every time
	Content string
}

// ParseSections parses memory.md content into a list of Sections.
// If no section markers are found, returns a single global/always section (backward-compatible).
func ParseSections(content string) []Section {
	matches := sectionHeaderRe.FindAllStringIndex(content, -1)
	if len(matches) == 0 {
		// Old format or hand-written file; treat the whole thing as an always section
		return []Section{{
			Name:    "global",
			Tags:    []string{"always"},
			Content: strings.TrimSpace(content),
		}}
	}

	sections := make([]Section, 0, len(matches))
	for i, match := range matches {
		sub := sectionHeaderRe.FindStringSubmatch(content[match[0]:match[1]])
		name := sub[1]

		rawTags := strings.Split(sub[2], ",")
		tags := make([]string, 0, len(rawTags))
		for _, t := range rawTags {
			t = strings.ToLower(strings.TrimSpace(t))
			if t != "" {
				tags = append(tags, t)
			}
		}

		// Section content: from end of header to start of next header
		start := match[1]
		end := len(content)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		body := strings.TrimSpace(content[start:end])
		if len(body) > maxSectionBytes {
			body = body[:maxSectionBytes] + "\n...(truncated)"
		}

		sections = append(sections, Section{Name: name, Tags: tags, Content: body})
	}
	return sections
}

// SelectRelevant selects relevant sections based on prompt keywords.
//
//   - Sections with the "always" tag are always included
//   - Other sections are sorted by tag hit count; top maxExtraSections are selected
func SelectRelevant(sections []Section, prompt string) []Section {
	promptLower := strings.ToLower(prompt)

	type candidate struct {
		sec   Section
		score int
	}

	var always []Section
	var others []candidate

	for _, sec := range sections {
		isAlways := false
		for _, t := range sec.Tags {
			if t == "always" {
				isAlways = true
				break
			}
		}
		if isAlways {
			always = append(always, sec)
			continue
		}

		// Calculate tag hit score (1 point per tag that appears in the prompt)
		score := 0
		for _, t := range sec.Tags {
			if t != "" && strings.Contains(promptLower, t) {
				score++
			}
		}
		if score > 0 {
			others = append(others, candidate{sec, score})
		}
	}

	// Sort by score descending (section count is usually small, simple insertion sort is fine)
	for i := 1; i < len(others); i++ {
		for j := i; j > 0 && others[j].score > others[j-1].score; j-- {
			others[j], others[j-1] = others[j-1], others[j]
		}
	}

	result := make([]Section, 0, len(always)+maxExtraSections)
	result = append(result, always...)
	for i, c := range others {
		if i >= maxExtraSections {
			break
		}
		result = append(result, c.sec)
	}
	return result
}

// BuildInjection concatenates the content of selected sections into an injection string.
func BuildInjection(sections []Section) string {
	if len(sections) == 0 {
		return ""
	}
	parts := make([]string, 0, len(sections))
	for _, s := range sections {
		if s.Content != "" {
			parts = append(parts, s.Content)
		}
	}
	return strings.Join(parts, "\n\n")
}
