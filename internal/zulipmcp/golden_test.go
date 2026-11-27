package zulipmcp

import (
	"encoding/json"
	"flag"
	"os"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata/tools.golden.json")

// TestToolsGolden renders the name, description and input schema of
// every tool and compares them with testdata/tools.golden.json. The
// golden file was made before the descriptions moved out of Go
// literals, so it proves the move changed no byte.
func TestToolsGolden(t *testing.T) {
	type entry struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Schema      map[string]any `json:"schema"`
	}
	var all []entry
	set := stateTools(t, func(string, string, string) (string, error) { return "", nil },
		func(string) (string, error) { return "", nil })
	for _, x := range set.Tools() {
		all = append(all, entry{x.Name, x.Description, x.Schema})
	}
	got, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	const path = "testdata/tools.golden.json"
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("tools differ from %s:\n%s", path, got)
	}
}
