package rollover

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func longChain(t *testing.T, p Poster) *Splitter {
	t.Helper()
	s, err := New(Config{Budget: 300, Poster: p})
	if err != nil {
		t.Fatal(err)
	}
	s.Append(strings.Repeat("line of text here\n", 60))
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRewriteDeletesSurplus(t *testing.T) {
	d := newDeleting()
	s := longChain(t, d)
	n := len(s.IDs())
	if err := s.Rewrite(context.Background(), "short\n"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close(context.Background(), "")
	if n < 2 || len(d.deleted) != n-1 || len(d.order) != 1 || d.bodies[d.order[0]] != "short\n" {
		t.Fatalf("n=%d deleted=%v bodies=%v", n, d.deleted, d.bodies)
	}
}

func TestRewriteBlanksSurplusWithoutDeleter(t *testing.T) {
	f := newFake()
	s := longChain(t, f)
	if err := s.Rewrite(context.Background(), "short\n"); err != nil {
		t.Fatal(err)
	}
	for _, id := range f.order[1:] {
		if f.bodies[id] != DefaultEmptyBody {
			t.Fatalf("surplus %d = %q", id, f.bodies[id])
		}
	}
}

func TestRewriteFallsBackAndReportsEditError(t *testing.T) {
	d := newDeleting()
	s := longChain(t, d)
	d.deleteErr = errors.New("no delete")
	d.editErr = errors.New("no edit")
	if err := s.Rewrite(context.Background(), "short\n"); err == nil {
		t.Fatal("want error")
	}
}

func TestRewriteSkipsUnpostedSurplus(t *testing.T) {
	f := newFake()
	s, _ := New(Config{Budget: 300, Poster: f})
	s.Append(strings.Repeat("line of text here\n", 60))
	if err := s.Rewrite(context.Background(), "short\n"); err != nil || f.posts != 0 {
		t.Fatalf("err=%v posts=%d", err, f.posts)
	}
}
