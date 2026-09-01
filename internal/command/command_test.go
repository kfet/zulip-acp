package command

import (
	"go/build"
	"reflect"
	"strings"
	"testing"
)

var testSet = NewSet(
	Spec{Name: "new", Aliases: []string{"reset"}, Summary: "start a fresh conversation"},
	Spec{Name: "model", Args: "[id]", Summary: "show or switch the model"},
	Spec{Name: "help", Summary: "list commands"},
)

func TestSpecsAreReturnedInRegistrationOrder(t *testing.T) {
	var got []string
	for _, s := range testSet.Specs() {
		got = append(got, s.Name)
	}
	want := []string{"new", "model", "help"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Specs() = %v, want %v", got, want)
	}
}

func TestSpecsIsACopy(t *testing.T) {
	s := testSet.Specs()
	s[0].Name = "clobbered"
	if testSet.Specs()[0].Name != "new" {
		t.Fatal("Specs() handed out the registry's own backing array")
	}
}

func TestLookupMatchesNameAliasAndCase(t *testing.T) {
	for _, name := range []string{"new", "NEW", "New", "reset", "ReSeT"} {
		got, ok := testSet.Lookup(name)
		if !ok || got.Name != "new" {
			t.Fatalf("Lookup(%q) = %v, %v; want the \"new\" spec", name, got, ok)
		}
	}
	if _, ok := testSet.Lookup("nope"); ok {
		t.Fatal("Lookup(\"nope\") reported a hit")
	}
}

func TestUsage(t *testing.T) {
	n, _ := testSet.Lookup("new")
	if got := n.Usage(); got != "!new" {
		t.Fatalf("Usage() = %q", got)
	}
	m, _ := testSet.Lookup("model")
	if got := m.Usage(); got != "!model [id]" {
		t.Fatalf("Usage() = %q", got)
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantKind Kind
		wantName string
		wantArgs string
		wantText string
	}{
		// Recognised commands.
		{"bare", "!new", KindCommand, "new", "", ""},
		{"alias", "!reset", KindCommand, "new", "", ""},
		{"case folded", "!NeW", KindCommand, "new", "", ""},
		{"with args", "!model  anthropic/opus ", KindCommand, "model", "anthropic/opus", ""},
		{"args keep inner spaces", "!model a  b", KindCommand, "model", "a  b", ""},
		{"newline separates", "!help\nrest", KindCommand, "help", "rest", ""},

		// Command-shaped but unknown: named, and never forwarded.
		{"unknown", "!frobnicate now", KindUnknown, "frobnicate", "", ""},
		{"unknown case folded", "!Frob", KindUnknown, "frob", "", ""},

		// Prose. Anything that is not command-shaped reaches the agent
		// byte-for-byte.
		{"plain prose", "hello there", KindProse, "", "", "hello there"},
		{"bang mid-sentence", "wow! do it", KindProse, "", "", "wow! do it"},
		{"lone bang", "!", KindProse, "", "", "!"},
		{"bang space", "! new", KindProse, "", "", "! new"},
		{"bang digit", "!5 minutes", KindProse, "", "", "!5 minutes"},
		{"bang punctuation", "!important: fix", KindProse, "", "", "!important: fix"},
		{"bang non-ascii", "!ünlaut", KindProse, "", "", "!ünlaut"},
		{"empty", "", KindProse, "", "", ""},

		// The escape: a leading "!!" yields prose starting with one "!".
		{"escape known", "!!new", KindProse, "", "", "!new"},
		{"escape prose", "!!important: fix", KindProse, "", "", "!important: fix"},
		{"escape alone", "!!", KindProse, "", "", "!"},
		{"triple bang", "!!!x", KindProse, "", "", "!!x"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := testSet.Parse(tc.in)
			if got.Kind != tc.wantKind {
				t.Fatalf("Kind = %v, want %v", got.Kind, tc.wantKind)
			}
			if got.Name != tc.wantName {
				t.Fatalf("Name = %q, want %q", got.Name, tc.wantName)
			}
			if got.Args != tc.wantArgs {
				t.Fatalf("Args = %q, want %q", got.Args, tc.wantArgs)
			}
			if got.Text != tc.wantText {
				t.Fatalf("Text = %q, want %q", got.Text, tc.wantText)
			}
		})
	}
}

func TestParseSurroundingWhitespaceIsIgnored(t *testing.T) {
	got := testSet.Parse("   !new   ")
	if got.Kind != KindCommand || got.Name != "new" {
		t.Fatalf("Parse = %+v", got)
	}
}

func TestParseCarriesTheSpec(t *testing.T) {
	got := testSet.Parse("!model x")
	if got.Spec.Summary != "show or switch the model" {
		t.Fatalf("Spec = %+v", got.Spec)
	}
}

func TestKindString(t *testing.T) {
	for k, want := range map[Kind]string{
		KindProse:   "prose",
		KindCommand: "command",
		KindUnknown: "unknown",
		Kind(99):    "Kind(99)",
	} {
		if got := k.String(); got != want {
			t.Fatalf("Kind(%d).String() = %q, want %q", k, got, want)
		}
	}
}

func TestDuplicateNamePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewSet accepted a duplicate name")
		}
	}()
	NewSet(
		Spec{Name: "a"},
		Spec{Name: "b", Aliases: []string{"a"}},
	)
}

// TestNoZulipImports enforces the rule this package exists under: the
// parse core is generic relay machinery, and staying free of every
// other internal package is what keeps promoting it to acp-kit a
// `git mv` rather than an untangling. See BACKLOG.md.
func TestNoZulipImports(t *testing.T) {
	p, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	for _, imp := range append(append([]string{}, p.Imports...), p.TestImports...) {
		if strings.Contains(imp, "zulip-acp/") {
			t.Fatalf("internal/command imports %q; it must stay Zulip-free", imp)
		}
	}
}
