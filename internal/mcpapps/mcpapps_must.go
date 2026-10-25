package mcpapps

import "fmt"

// mustJSON asserts that json.Marshal cannot fail on what this package
// hands it.
//
// It cannot be reached from a test. Every value marshalled here either
// came out of encoding/json a moment earlier (an ACP notification, a
// json.RawMessage the agent sent) or is a map, slice or struct of
// strings, numbers and such raw messages. Marshal fails only on
// channels, functions, complex numbers, cyclic values and a
// json.RawMessage that is not valid JSON, and none of those can occur.
// A panic is how a future change that breaks this is found at once.
func mustJSON(b []byte, err error) []byte {
	if err != nil {
		panic(fmt.Sprintf("mcpapps: marshal of a JSON-derived value failed: %v", err))
	}
	return b
}
