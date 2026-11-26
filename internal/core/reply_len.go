package core

import (
	"regexp"
	"strings"
	"unicode"
)

var (
	mdLinkRe       = regexp.MustCompile(`\[([^\]]+)\]\((?:[^()\s]+|\([^()\s]*\))*\)`)
	mdLinePrefixRe = regexp.MustCompile(`^\s*(?:#{1,6}\s+|>\s*|[-*•]+\s+|\d+(?:[.)]\s+|）\s*))`)
)

// ReadableReplyLen measures a reply's human-perceived length:
//   - each CJK character (Han/Kana/Hangul) counts as 1
//   - each contiguous non-CJK word, version, branch, path, or URL counts as 1
//   - newlines, Markdown syntax (#, -, *, **, `, >, code fences, link URLs), and pure punctuation count as 0
func ReadableReplyLen(s string) int {
	if strings.TrimSpace(s) == "" {
		return 0
	}
	total := 0
	for _, rawLine := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" || strings.HasPrefix(trimmed, "```") {
			continue
		}
		// Skip Markdown table separator rows like |---|---|
		if strings.Contains(trimmed, "|") && isTableSeparatorLine(trimmed) {
			continue
		}
		// Replace [label](url) with label so URLs/file paths in Markdown links do not inflate budget.
		line := mdLinkRe.ReplaceAllString(rawLine, "$1")
		line = mdLinePrefixRe.ReplaceAllString(line, "")
		total += countReadableRunes(line)
	}
	return total
}

func isTableSeparatorLine(s string) bool {
	hasDash := false
	for _, r := range s {
		switch r {
		case '|', ':', ' ', '\t':
		case '-':
			hasDash = true
		default:
			return false
		}
	}
	return hasDash
}

func isCJKLetter(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}

func isWordJoiner(r rune) bool {
	switch r {
	case '.', '_', '-', '/', ':', '+', '@', '~':
		return true
	default:
		return false
	}
}

func isNonCJKWordRune(r rune) bool {
	return !isCJKLetter(r) && (unicode.IsLetter(r) || unicode.IsDigit(r))
}

func countReadableRunes(line string) int {
	runes := []rune(line)
	n := 0
	inWord := false
	for i, r := range runes {
		if isCJKLetter(r) {
			n++
			inWord = false
			continue
		}
		if isNonCJKWordRune(r) {
			if !inWord {
				n++
				inWord = true
			}
			continue
		}
		if inWord && isWordJoiner(r) && i+1 < len(runes) && isNonCJKWordRune(runes[i+1]) {
			continue
		}
		inWord = false
	}
	return n
}
