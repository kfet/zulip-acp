package config

//go:generate go run ../../cmd/schemagen

import (
	"fmt"
	"io"
	"strings"
)

// SchemaField documents one config key. Schema (schema_gen.go) holds
// one per key, generated from the doc comments of the config structs.
type SchemaField struct{ Key, Type, Doc string }

// WriteSchema writes the config key reference: every key, its type and
// its doc. A key that starts with filter is kept; "" keeps all.
func WriteSchema(w io.Writer, filter string) int {
	n := 0
	for _, f := range Schema {
		if !strings.HasPrefix(f.Key, filter) {
			continue
		}
		fmt.Fprintf(w, "%s (%s)\n    %s\n", f.Key, f.Type, f.Doc)
		n++
	}
	return n
}
