package sysprompt

import (
	"bufio"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Notes index limits. They bound what the index can add to EVERY
// session's system prompt, whatever the operator keeps in the store.
const (
	notesMaxEntries = 60
	notesMaxSummary = 120  // code points per summary line
	notesMaxBlock   = 6000 // code points for the whole block
	notesHeadBytes  = 2048 // bytes read from each note to find a summary
	notesMaxDepth   = 6    // directory levels; also stops symlink loops
)

// NotesIndex renders the notes block of the system prompt: a rule and an
// index of every note in fsys, one line per file with its summary.
//
// Why it exists (incident 2026-11-09): the notes store was pull-only.
// The agent read it only "when you do not know", but an agent does not
// know what it does not know: it acted on a plausible guess about the
// fleet while a note held the real answer. A store whose content the
// agent cannot see at decision time is never consulted. The index puts
// WHAT the notes cover in front of the agent every session, so a
// matching note is visible before the guess is made. It is generated
// from the store, so it holds for any fact anyone writes down.
//
// dir is the absolute path the agent uses to read the notes. An empty
// store gives only the rule, so the agent still knows where to write.
func NotesIndex(fsys fs.FS, dir string) string {
	files := listNotes(fsys, ".", 0)
	sort.Strings(files)

	var b strings.Builder
	b.WriteString("Notes — your persistent memory, in `" + dir + "`:\n")
	b.WriteString("- Earlier sessions, the operator and you wrote these down because the obvious guess was wrong or unknown. A note OVERRIDES your assumptions.\n")
	b.WriteString("- Before you act on any fact about the environment — which host, where code lives, how to build, deploy, update, configure or reach something, what the user prefers — find the notes below that cover it and read them. Do this also when you feel sure: that feeling is how notes get skipped.\n")
	b.WriteString("- Ask the user only what the notes do not answer. When you learn such a fact, write or update a note.\n")
	if len(files) == 0 {
		b.WriteString("\nThe store is empty.")
		return b.String()
	}
	b.WriteString("\nIndex (file — summary):\n")
	shown := 0
	for _, f := range files {
		line := "- " + f
		if s := noteSummary(fsys, f); s != "" {
			line += " — " + s
		}
		line += "\n"
		if shown == notesMaxEntries || utf8.RuneCountInString(b.String())+utf8.RuneCountInString(line) > notesMaxBlock {
			break
		}
		b.WriteString(line)
		shown++
	}
	if more := len(files) - shown; more > 0 {
		b.WriteString("- … and " + strconv.Itoa(more) + " more: list the directory or grep it.\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// noteSummary returns a one-line summary of a note: its frontmatter
// description, else its first heading, else its first text line.
func noteSummary(fsys fs.FS, p string) string {
	f, err := fsys.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, notesHeadBytes)
	n, _ := f.Read(head)
	sc := bufio.NewScanner(strings.NewReader(string(head[:n])))
	inFront, first, fallback := false, true, ""
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if first && l == "---" {
			inFront, first = true, false
			continue
		}
		first = false
		if inFront {
			if l == "---" {
				inFront = false
			} else if v, ok := strings.CutPrefix(l, "description:"); ok {
				return clip(strings.Trim(strings.TrimSpace(v), `"'`))
			}
			continue
		}
		if h, ok := strings.CutPrefix(l, "#"); ok {
			return clip(strings.TrimSpace(strings.TrimLeft(h, "#")))
		}
		if fallback == "" && l != "" {
			fallback = l
		}
	}
	return clip(fallback)
}

// clip cuts s to notesMaxSummary code points.
func clip(s string) string {
	if utf8.RuneCountInString(s) <= notesMaxSummary {
		return s
	}
	r := []rune(s)
	return string(r[:notesMaxSummary-1]) + "…"
}

// listNotes returns the .md files under dir, depth first. It follows
// symlinked directories (a shared fleet/ store is often one), which
// fs.WalkDir does not, and stops at notesMaxDepth so a symlink loop
// ends. Entries whose name starts with "." are skipped. A directory it
// cannot read only loses its entries.
func listNotes(fsys fs.FS, dir string, depth int) []string {
	ents, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		p := path.Join(dir, name)
		isDir := e.IsDir()
		if e.Type()&fs.ModeSymlink != 0 {
			st, err := fs.Stat(fsys, p)
			if err != nil {
				continue
			}
			isDir = st.IsDir()
		}
		switch {
		case isDir && depth < notesMaxDepth:
			out = append(out, listNotes(fsys, p, depth+1)...)
		case !isDir && strings.EqualFold(path.Ext(name), ".md"):
			out = append(out, p)
		}
	}
	return out
}
