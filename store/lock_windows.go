package store

import "os"

func lockFile(f *os.File) error {
	// Best-effort on Windows: lock files are a single-writer advisory hint.
	return nil
}

func unlockFile(f *os.File) error {
	return nil
}
