package stream

import (
	"os"

	"github.com/huchangjun118/diff-merge-engine-go/derr"
)

// syncDir fsyncs a directory so a rename within it is durable. On platforms
// where directory sync is unsupported it is a best-effort no-op.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return derr.Wrap(derr.KindCorrupt, "cannot open output directory", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		if os.IsPermission(err) {
			return nil
		}
		return derr.Wrap(derr.KindCorrupt, "cannot sync output directory", err)
	}
	return nil
}
