package main

import (
	"strings"
	"unicode"
)

// dedupKey groups results that are the same book offered by different bots.
func dedupKey(author, title, format string) string {
	return normalizeText(author) + "|" + normalizeText(title) + "|" + format
}

// normalizeText lowercases s and collapses runs of non-alphanumeric
// characters to single spaces, so minor punctuation/spacing differences
// between bots don't defeat matching.
func normalizeText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			prevSpace = false
		} else if !prevSpace {
			b.WriteRune(' ')
			prevSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

// scoreRelevance ranks how well a query matches a result's author/title.
// It's a simple, bounded, linear score — deliberately not a fuzzy-matching
// library: those are tuned for short filenames/code symbols and can fail
// outright (or blow up exponentially) on noisy multi-word book metadata.
// An exact or substring title match always outranks incidental word
// overlap elsewhere in the text, and trailing noise (edition tags, "(Retail)",
// series markers) never tanks a genuinely exact match the way a
// per-character length penalty would.
func scoreRelevance(query, author, title string) int {
	q := normalizeText(query)
	if q == "" {
		return 0
	}
	normTitle := normalizeText(title)
	normCombined := normalizeText(author) + " " + normTitle

	score := 0
	switch {
	case normTitle == q:
		score += 1000
	case strings.Contains(normTitle, q):
		score += 700
	case strings.Contains(normCombined, q):
		score += 400
	}

	qWords := strings.Fields(q)
	if len(qWords) > 0 {
		matched := 0
		for _, w := range qWords {
			if strings.Contains(normCombined, w) {
				matched++
			}
		}
		score += 300 * matched / len(qWords)
	}

	return score
}
