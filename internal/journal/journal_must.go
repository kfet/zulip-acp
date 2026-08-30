package journal

import "encoding/json"

// mustRandom fills b with cryptographically secure random bytes.
//
// Since Go 1.24 crypto/rand.Read never returns an error — it panics
// internally if the OS entropy source fails, because a program that
// cannot get randomness has no safe way to continue. There is
// therefore no reachable error branch for a test to cover, and
// conv-ids must never fall back to a predictable source.
func mustRandom(b []byte) {
	if _, err := randomRead(b); err != nil {
		panic("journal: crypto/rand failed: " + err.Error())
	}
}

// mustMarshal encodes the journal file.
//
// The value is a closed struct of ints and strings; encoding/json
// cannot fail on it. Returning the error would leave a branch no test
// could reach.
func mustMarshal(f file) []byte {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		panic("journal: unmarshalable journal: " + err.Error())
	}
	return b
}
