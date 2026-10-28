package config

import "os"

// mustStat stats a file that is already open. Fstat on a descriptor
// the process holds cannot fail short of a kernel fault, and a test
// has no way to make it fail.
func mustStat(f *os.File) os.FileInfo {
	fi, err := f.Stat()
	if err != nil {
		panic(err)
	}
	return fi
}
