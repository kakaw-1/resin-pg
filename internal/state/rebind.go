package state

import (
	"strconv"
	"strings"
)

// SQLRebind converts question-mark placeholders (the SQLite/MySQL style used
// throughout Resin's repos) into PostgreSQL positional placeholders ($1, $2...).
//
// The rewrite is lexical rather than a blind string replace: '?' inside quoted
// literals, quoted identifiers, line/block comments and dollar-quoted strings is
// left untouched. Already-positional text ($1) survives unchanged, so the
// function is idempotent for queries that were written for PostgreSQL directly.
//
// PostgreSQL also uses '?' as a JSONB existence operator. Resin does not use that
// operator in persisted SQL; if that ever changes, keep such queries out of the
// rebinding path.
func SQLRebind(query string) string {
	if !strings.ContainsRune(query, '?') {
		return query
	}

	var b strings.Builder
	b.Grow(len(query) + 8)

	n := 0
	for i := 0; i < len(query); {
		switch query[i] {
		case '\'', '"':
			end := skipQuoted(query, i, query[i])
			b.WriteString(query[i:end])
			i = end
		case '-':
			if i+1 < len(query) && query[i+1] == '-' {
				end := strings.IndexByte(query[i:], '\n')
				if end < 0 {
					b.WriteString(query[i:])
					i = len(query)
					continue
				}
				b.WriteString(query[i : i+end])
				i += end
				continue
			}
			b.WriteByte(query[i])
			i++
		case '/':
			if i+1 < len(query) && query[i+1] == '*' {
				end := strings.Index(query[i+2:], "*/")
				if end < 0 {
					b.WriteString(query[i:])
					i = len(query)
					continue
				}
				stop := i + 2 + end + 2
				b.WriteString(query[i:stop])
				i = stop
				continue
			}
			b.WriteByte(query[i])
			i++
		case '$':
			if end := skipDollarQuoted(query, i); end > i {
				b.WriteString(query[i:end])
				i = end
				continue
			}
			b.WriteByte(query[i])
			i++
		case '?':
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			i++
		default:
			b.WriteByte(query[i])
			i++
		}
	}
	return b.String()
}

// skipQuoted returns the index just past a quoted region starting at start.
// Doubled quotes (” or "") are escapes and stay inside the region.
func skipQuoted(query string, start int, quote byte) int {
	j := start + 1
	for j < len(query) {
		if query[j] != quote {
			j++
			continue
		}
		if j+1 < len(query) && query[j+1] == quote {
			j += 2
			continue
		}
		return j + 1
	}
	return len(query)
}

// skipDollarQuoted returns the index just past a dollar-quoted string ($$...$$ or
// $tag$...$tag$) starting at start, or start when the position is not a
// dollar-quote delimiter.
func skipDollarQuoted(query string, start int) int {
	j := start + 1
	for j < len(query) && isDollarTagByte(query[j]) {
		j++
	}
	if j >= len(query) || query[j] != '$' {
		return start
	}
	delim := query[start : j+1]
	rest := query[j+1:]
	if closeAt := strings.Index(rest, delim); closeAt >= 0 {
		return j + 1 + closeAt + len(delim)
	}
	// Unterminated dollar quote: treat the remainder as literal text.
	return len(query)
}

func isDollarTagByte(c byte) bool {
	return c == '_' ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9')
}
