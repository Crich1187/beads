package linear

import (
	"strings"
)

// linearDescriptionsEqual reports whether a description bd would push and the
// description Linear currently stores are the same document once Linear's
// own markdown re-serialization is discounted.
//
// Linear does not store the description markdown byte-for-byte: it parses
// and re-serializes it. Comparing raw text therefore reports a difference on
// every cycle for any description Linear rewrote, and each such "difference"
// re-sends an unchanged description and bumps the issue's updatedAt
// (acceptance run 1, finding F2: TEST-4, the Needs Carl template).
//
// This is used ONLY to decide whether a push can be skipped. It never changes
// what is pushed: when the texts differ after normalization, the local text is
// sent exactly as it is.
func linearDescriptionsEqual(local, remote string) bool {
	if local == remote {
		return true
	}
	return normalizeLinearDescriptionForCompare(local) == normalizeLinearDescriptionForCompare(remote)
}

// normalizeLinearDescriptionForCompare canonicalizes the parts of a markdown
// description that Linear rewrites without changing the document:
//
//   - line endings: CRLF and lone CR become LF;
//   - trailing whitespace on every line is dropped;
//   - leading and trailing blank lines are dropped;
//   - an ATX heading line ("#" to "######") is followed by exactly one blank
//     line (observed: Linear stores "## Options\n\n1. One." for a pushed
//     "## Options\n1. One."), and a run of blank lines after a heading
//     collapses to one;
//   - backslash-escaped square brackets become plain brackets (observed:
//     Linear stores "\[acc22 ...\]" for a sent "[acc22 ...]").
//
// Lines inside fenced code blocks (``` or ~~~) keep their content: only their
// line endings and trailing whitespace are normalized. Paragraph text, single
// line breaks and blank lines elsewhere are left alone, so a changed word, an
// added line or a removed paragraph still compares as a difference.
func normalizeLinearDescriptionForCompare(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")

	out := make([]string, 0, len(lines)+4)
	var fence string // opening fence marker while inside a fenced code block
	afterHeading := false
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")

		if fence != "" {
			out = append(out, line)
			if isClosingFence(line, fence) {
				fence = ""
			}
			continue
		}
		if marker := openingFence(line); marker != "" {
			if afterHeading {
				out = append(out, "")
				afterHeading = false
			}
			fence = marker
			out = append(out, line)
			continue
		}

		if afterHeading {
			if line == "" {
				continue // collapse blank lines after a heading; one is re-added below
			}
			out = append(out, "")
			afterHeading = false
		}

		line = strings.ReplaceAll(line, `\[`, "[")
		line = strings.ReplaceAll(line, `\]`, "]")
		out = append(out, line)
		if isATXHeading(line) {
			afterHeading = true
		}
	}

	// Drop leading and trailing blank lines.
	start, end := 0, len(out)
	for start < end && out[start] == "" {
		start++
	}
	for end > start && out[end-1] == "" {
		end--
	}
	return strings.Join(out[start:end], "\n")
}

// isATXHeading reports whether line is a markdown ATX heading: up to three
// spaces of indentation, one to six '#', then a space or end of line.
func isATXHeading(line string) bool {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return false
	}
	level := 0
	for level < len(trimmed) && trimmed[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return false
	}
	return level == len(trimmed) || trimmed[level] == ' ' || trimmed[level] == '\t'
}

// openingFence returns the fence marker ("```" or "~~~", possibly longer) when
// line opens a fenced code block, or "".
func openingFence(line string) string {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return ""
	}
	for _, ch := range []byte{'`', '~'} {
		n := 0
		for n < len(trimmed) && trimmed[n] == ch {
			n++
		}
		if n >= 3 {
			return strings.Repeat(string(ch), n)
		}
	}
	return ""
}

// isClosingFence reports whether line closes a block opened with marker: the
// same fence character, at least as long, and nothing else on the line.
func isClosingFence(line, marker string) bool {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < len(marker) {
		return false
	}
	return strings.Trim(trimmed, marker[:1]) == ""
}
