package stream

import (
	"bytes"
	"io"
	"os"

	"github.com/huchangjun118/diff-merge-engine-go/derr"
)

// spool is a private write destination that stays in memory until it exceeds
// spoolLimit, after which it spills to a temporary file. Read methods replay
// the buffered result. On failure the spool is discarded without the caller
// ever observing partial output.
type spool struct {
	mem   *bytes.Buffer
	file  *os.File
	name  string
	spent int64
}

func newSpool() (*spool, error) {
	return &spool{mem: new(bytes.Buffer)}, nil
}

func (s *spool) Write(p []byte) (int, error) {
	if s.file != nil {
		n, err := s.file.Write(p)
		s.spent += int64(n)
		if err != nil {
			return n, derr.Wrap(derr.KindCorrupt, "cannot write spool file", err)
		}
		return n, nil
	}
	n, err := s.mem.Write(p)
	if err != nil {
		return n, derr.Wrap(derr.KindCorrupt, "cannot write spool", err)
	}
	s.spent += int64(n)
	if s.spent > spoolLimit {
		f, ferr := os.CreateTemp("", "diff-merge-spool-*")
		if ferr != nil {
			return n, derr.Wrap(derr.KindCorrupt, "cannot create spool file", ferr)
		}
		if _, werr := f.Write(s.mem.Bytes()); werr != nil {
			f.Close()
			os.Remove(f.Name())
			return n, derr.Wrap(derr.KindCorrupt, "cannot spill spool", werr)
		}
		s.file = f
		s.name = f.Name()
		s.mem = nil
	}
	return n, nil
}

// seekStart rewinds the spool so it can be read from the beginning.
func (s *spool) seekStart() (int64, error) {
	if s.file != nil {
		if _, err := s.file.Seek(0, io.SeekStart); err != nil {
			return 0, derr.Wrap(derr.KindCorrupt, "cannot rewind spool", err)
		}
		return s.spent, nil
	}
	return int64(s.mem.Len()), nil
}

func (s *spool) Read(p []byte) (int, error) {
	if s.file != nil {
		return s.file.Read(p)
	}
	return s.mem.Read(p)
}

// cleanup removes any spilled temp file. Safe to call multiple times.
func (s *spool) cleanup() {
	if s.file != nil {
		s.file.Close()
		os.Remove(s.name)
		s.file = nil
	}
}
