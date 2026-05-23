package gpu

import "os"

// defaultReadFile is the production implementation of ReadFileFunc.
// Lives in its own file so tests can compare references when injecting
// alternative readers.
func defaultReadFile(name string) ([]byte, error) {
	return os.ReadFile(name)
}
