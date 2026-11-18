package rollover

import (
	"context"
	"testing"
)

func TestFixTables(t *testing.T) {
	tbl := "| a | b |\n|---|---|\n| 1 | 2 |"
	for _, tc := range []struct{ name, in, want string }{
		{"label above", "**T**\n" + tbl, "**T**\n\n" + tbl},
		{"text below", tbl + "\nReady.", tbl + "\n\nReady."},
		{"both", "x\n" + tbl + "\ny", "x\n\n" + tbl + "\n\ny"},
		{"already fine", "x\n\n" + tbl + "\n\ny", "x\n\n" + tbl + "\n\ny"},
		{"table first", tbl, tbl},
		{"no separator", "a | b\nc | d", "a | b\nc | d"},
		{"prose pipe", "use a|b here\nnext", "use a|b here\nnext"},
		{"in fence", "```text\nx\n" + tbl + "\n```", "```text\nx\n" + tbl + "\n```"},
		{"after fence", "```\ncode\n```\nx\n" + tbl, "```\ncode\n```\nx\n\n" + tbl},
		{"no leading pipes", "x\na | b\n--- | :---:\n1 | 2", "x\n\na | b\n--- | :---:\n1 | 2"},
		{"short dashes", "x\n|a|\n|--|", "x\n|a|\n|--|"},
		{"pipeless separator", "x\n|a|\n|   |", "x\n|a|\n|   |"},
		{"two tables", tbl + "\n" + tbl, tbl + "\n" + tbl},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := FixTables(tc.in)
			if got != tc.want {
				t.Fatalf("FixTables(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if again := FixTables(got); again != got {
				t.Fatalf("not idempotent: %q", again)
			}
		})
	}
}

func TestFlushFixesTables(t *testing.T) {
	p := newFake()
	s, err := New(Config{Poster: p})
	if err != nil {
		t.Fatal(err)
	}
	s.Append("**T**\n| a |\n|---|\n| 1 |\nok")
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := p.bodies[p.order[len(p.order)-1]], "**T**\n\n| a |\n|---|\n| 1 |\n\nok"; got != want {
		t.Fatalf("posted %q, want %q", got, want)
	}
	if s.Pending() {
		t.Fatal("fixed body must not leave the message dirty")
	}
}
