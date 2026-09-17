package statusline

import (
	"strings"
	"testing"
)

func TestSpinner_RendersNoticeOnItsOwnLine(t *testing.T) {
	got := Spinner(Status{Mood: "steady"}, "..", "⏳ retrying in 30s")

	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), got)
	}
	if !strings.Contains(lines[0], "Thinking..") || !strings.Contains(lines[0], "steady") {
		t.Errorf("spinner line lost its segments: %q", lines[0])
	}
	if lines[1] != "> *⏳ retrying in 30s*" {
		t.Errorf("notice line = %q", lines[1])
	}
}

func TestSpinner_NoNoticeIsOneLine(t *testing.T) {
	got := Spinner(Status{}, "..", "   ")
	if strings.Contains(got, "\n") {
		t.Errorf("blank notice added a line: %q", got)
	}
}

func TestSpinner_NoticeIsFlattenedAndCapped(t *testing.T) {
	got := Spinner(Status{}, ".", "line one\nline two")
	if !strings.Contains(got, "line one line two") {
		t.Errorf("notice not flattened: %q", got)
	}

	long := strings.Repeat("é", 400)
	got = Spinner(Status{}, ".", long)
	notice := strings.Split(got, "\n")[1]
	if r := []rune(notice); len(r) > 170 {
		t.Errorf("notice not capped: %d runes", len(r))
	}
	if !strings.HasSuffix(notice, "…*") {
		t.Errorf("expected ellipsis on truncation: %q", notice)
	}
}
