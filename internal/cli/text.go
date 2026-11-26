package cli

import "strings"

// normalizeEscapedNewlines expands unescaped literal \r\n and \n sequences into real LF (0x0A)
// when text contains no internal real newlines (for example, POSIX double-quoted CLI arguments
// like `corgi reply "a\n\nb"` or single-line bash `echo "a\n\nb" | corgi reply -`).
//
// It preserves text verbatim when:
//   - real internal newlines already exist (e.g. stdin heredoc or multiline input),
//   - \n appears inside inline backtick code spans (`...` or “...“),
//   - \n is preceded by an extra backslash (\\n), or
//   - \n starts a Windows drive path segment (e.g. C:\new\file.txt).
func normalizeEscapedNewlines(s string) string {
	trimmed := strings.TrimRight(s, "\r\n")
	if strings.ContainsAny(trimmed, "\r\n") {
		return s
	}
	if !strings.Contains(s, `\n`) {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	inlineCodeTicks := 0

	for i := 0; i < len(s); {
		if s[i] == '`' {
			j := i
			for j < len(s) && s[j] == '`' {
				j++
			}
			run := j - i
			if inlineCodeTicks > 0 {
				if run == inlineCodeTicks {
					inlineCodeTicks = 0
				}
			} else if run < 3 && hasClosingBackticks(s, j, run) {
				inlineCodeTicks = run
			}
			b.WriteString(s[i:j])
			i = j
			continue
		}

		if inlineCodeTicks == 0 && s[i] == '\\' {
			j := i
			for j < len(s) && s[j] == '\\' {
				j++
			}
			if j-i > 1 {
				b.WriteString(s[i:j])
				i = j
				continue
			}
			if i+3 < len(s) && s[i+1] == 'r' && s[i+2] == '\\' && s[i+3] == 'n' && !isWindowsDriveEscape(s, i) {
				b.WriteByte('\n')
				i += 4
				continue
			}
			if i+1 < len(s) && s[i+1] == 'n' && !isWindowsDriveEscape(s, i) {
				b.WriteByte('\n')
				i += 2
				continue
			}
		}

		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func hasClosingBackticks(s string, from, run int) bool {
	for i := from; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == '`' {
			j++
		}
		if j-i == run {
			return true
		}
		i = j
	}
	return false
}

func isWindowsDriveEscape(s string, idx int) bool {
	// Walk back to the start of the current path-like token to detect Windows drive paths
	// such as C:\new\notes.md (covering both the first segment and nested \notes segments).
	start := idx
	for start > 0 && isWindowsPathTokenChar(s[start-1]) {
		start--
	}
	if start+2 >= len(s) || !isASCIIAlpha(s[start]) || s[start+1] != ':' || s[start+2] != '\\' {
		return false
	}
	if start > 0 && isASCIIAlphaNum(s[start-1]) {
		return false
	}
	next := idx + 2
	if idx+3 < len(s) && s[idx+1] == 'r' && s[idx+2] == '\\' && s[idx+3] == 'n' {
		next = idx + 4
	}
	return next < len(s) && (isASCIIAlphaNum(s[next]) || s[next] == '_')
}

func isWindowsPathTokenChar(c byte) bool {
	return isASCIIAlphaNum(c) || c == ':' || c == '\\' || c == '/' || c == '.' || c == '_' || c == '-'
}

func isASCIIAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isASCIIAlphaNum(c byte) bool {
	return isASCIIAlpha(c) || (c >= '0' && c <= '9')
}
