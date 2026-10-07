package store

import (
	"os"

	"github.com/huchangjun118/diff-merge-engine-go/derr"
)

func readDirIfExists(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, derr.Wrap(derr.KindCorrupt, "cannot read directory: "+dir, err)
	}
	return entries, nil
}

func osRemoveAll(path string) { _ = os.RemoveAll(path) }
