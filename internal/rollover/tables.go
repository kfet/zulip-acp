package rollover

import "strings"

// FixTables puts a blank line before and after each pipe table in a
// message body, so that Zulip renders it.
//
// Zulip's markdown starts a table only at the start of a block. A label
// line directly above the header row ("**Timeline**" then "| a | b |")
// turns the whole table into literal text in that paragraph. Text on
// the line directly below the last row joins the table as one more
// row. Agents do both often, so the relay fixes it before it posts.
//
// A table is found only where a pipe line is followed by a separator
// row ("|---|---|"), so prose that contains a pipe is not changed.
// Lines inside fenced code blocks are not changed. The function is
// idempotent: a second pass adds nothing.
func FixTables(body string) string {
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines)+4)
	var st fence
	inTable := false
	for i, line := range lines {
		if st.open {
			out = append(out, line)
			st = scanFence(st, line)
			continue
		}
		blank := strings.TrimSpace(line) == ""
		switch {
		case inTable && isPipeRow(line):
			// Still in the table.
		case inTable:
			inTable = false
			if !blank {
				out = append(out, "")
			}
		}
		if !inTable && isPipeRow(line) && i+1 < len(lines) && isSeparatorRow(lines[i+1]) {
			if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
				out = append(out, "")
			}
			inTable = true
		}
		out = append(out, line)
		st = scanFence(st, line)
	}
	return strings.Join(out, "\n")
}

// isPipeRow reports whether line can be a table row.
func isPipeRow(line string) bool {
	t := strings.TrimSpace(line)
	return t != "" && strings.Contains(t, "|") && !strings.HasPrefix(t, "```")
}

// isSeparatorRow reports whether line is a table header separator such
// as "|---|:--:|" or "--- | ---".
func isSeparatorRow(line string) bool {
	t := strings.Trim(strings.TrimSpace(line), "|")
	if !strings.Contains(t, "-") {
		return false
	}
	cells := strings.Split(t, "|")
	for _, c := range cells {
		c = strings.Trim(strings.TrimSpace(c), ":")
		if len(c) < 3 || strings.Trim(c, "-") != "" {
			return false
		}
	}
	return true
}
