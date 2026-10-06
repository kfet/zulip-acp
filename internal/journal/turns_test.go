package journal

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTurnLeafMapsMessagesToTheirTurn(t *testing.T) {
	j, path := tmpJournal(t)
	c, err := j.Ensure(Channel(1, "t"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range []Turn{
		{PromptID: 10, ReplyID: 11, Leaf: "a"},
		{PromptID: 20, ReplyID: 22, Leaf: "b"},
		{ReplyID: 30, Leaf: "c"},                                  // no prompt message: keyed by its reply
		{PromptID: 40, Leaf: "d"},                                 // abstained: no reply
		{PromptID: 50, ReplyID: 51},                               // no leaf: ignored
		{Leaf: "e"},                                               // no message: ignored
		{PromptID: 60, Leaf: strings.Repeat("x", MaxLeafBytes+1)}, // not an id: ignored
	} {
		if err := j.RecordTurn(c.ID, tr); err != nil {
			t.Fatal(err)
		}
	}
	j2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range []struct {
		msg  int64
		want string
	}{
		{9, ""}, {10, "a"}, {11, "a"}, {15, "a"}, {20, "b"}, {25, "b"},
		{30, "c"}, {39, "c"}, {40, "d"}, {51, "d"}, {99, "d"},
	} {
		if got := j2.TurnLeaf(c.ID, x.msg); got != x.want {
			t.Errorf("TurnLeaf(%d) = %q, want %q", x.msg, got, x.want)
		}
	}
	if got := j2.TurnLeaf("nope", 10); got != "" {
		t.Errorf("unknown conv = %q", got)
	}
	if err := j.RecordTurn("nope", Turn{PromptID: 1, Leaf: "x"}); err == nil {
		t.Error("want unknown-conversation error")
	}
}

func TestRecordTurnIsBoundedAndNotInheritedByNew(t *testing.T) {
	j, _ := tmpJournal(t)
	k := Channel(1, "t")
	c, _ := j.Ensure(k)
	for i := 1; i <= MaxTurns+5; i++ {
		if err := j.RecordTurn(c.ID, Turn{PromptID: int64(i), Leaf: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := j.LookupID(c.ID)
	if len(got.Turns) != MaxTurns || got.Turns[0].PromptID != 6 {
		t.Fatalf("turns = %d, first %d", len(got.Turns), got.Turns[0].PromptID)
	}
	if l := j.TurnLeaf(c.ID, 3); l != "" {
		t.Fatalf("dropped turn still maps: %q", l)
	}
	_, fresh, _, err := j.Retire(k)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.Turns) != 0 {
		t.Fatalf("fresh conversation inherited %d turns", len(fresh.Turns))
	}
}

func TestRecordTurnRollsBackOnAWriteFailure(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(filepath.Join(dir, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := j.Ensure(Channel(4, "topic"))
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := j.RecordTurn(c.ID, Turn{PromptID: 1, Leaf: "a"}); err == nil {
		t.Fatal("want write error")
	}
	if l := j.TurnLeaf(c.ID, 1); l != "" {
		t.Fatalf("leaf = %q after a failed write", l)
	}
}
