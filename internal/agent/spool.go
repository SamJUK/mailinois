package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const spoolExt = ".json.gz"

// spool holds batches on disk as one gzipped JSON file each, the request body as it will be sent. File names
// start with the time the batch was made, so sorting them by name gives the order to send them in.
type spool struct {
	dir   string
	limit int64
	wake  chan struct{} // signalled on every put

	mu       sync.Mutex
	files    []spooled // oldest first
	total    int64
	inflight string // being sent: never dropped for the limit
	seq      int
}

type spooled struct {
	name string
	size int64
}

func openSpool(dir string, limit int64) (*spool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir) // sorted by name
	if err != nil {
		return nil, err
	}
	s := &spool{dir: dir, limit: limit, wake: make(chan struct{}, 1)}
	for _, e := range entries {
		switch name := e.Name(); {
		case strings.HasSuffix(name, ".tmp"):
			os.Remove(filepath.Join(dir, name)) // cut short by a crash
		case strings.HasSuffix(name, spoolExt) && e.Type().IsRegular():
			if info, err := e.Info(); err == nil {
				s.files = append(s.files, spooled{name, info.Size()})
				s.total += info.Size()
			}
		}
	}
	return s, nil
}

func (s *spool) newName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return fmt.Sprintf("%019d-%06d%s", time.Now().UnixNano(), s.seq, spoolExt)
}

// put writes a batch, then drops the oldest ones while the spool is over its limit, and returns those.
func (s *spool) put(name string, body []byte) ([]spooled, error) {
	tmp := filepath.Join(s.dir, name+".tmp")
	err := os.WriteFile(tmp, body, 0o600)
	if err == nil {
		err = os.Rename(tmp, filepath.Join(s.dir, name))
	}
	if err != nil {
		os.Remove(tmp)
		return nil, err
	}

	s.mu.Lock()
	i, _ := slices.BinarySearchFunc(s.files, name, func(f spooled, name string) int { return strings.Compare(f.name, name) })
	s.files = slices.Insert(s.files, i, spooled{name, int64(len(body))})
	s.total += int64(len(body))
	var dropped []spooled
	for i := 0; s.total > s.limit && i < len(s.files); {
		f := s.files[i]
		if f.name == s.inflight {
			i++
			continue
		}
		os.Remove(filepath.Join(s.dir, f.name))
		s.files = slices.Delete(s.files, i, i+1)
		s.total -= f.size
		dropped = append(dropped, f)
	}
	s.mu.Unlock()

	select {
	case s.wake <- struct{}{}:
	default:
	}
	return dropped, nil
}

// oldest names the next batch to send and keeps it from being dropped until release.
func (s *spool) oldest() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.files) == 0 {
		return "", false
	}
	s.inflight = s.files[0].name
	return s.inflight, true
}

func (s *spool) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inflight = ""
}

func (s *spool) read(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.dir, name))
}

func (s *spool) remove(name string) {
	os.Remove(filepath.Join(s.dir, name))
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.IndexFunc(s.files, func(f spooled) bool { return f.name == name }); i >= 0 {
		s.total -= s.files[i].size
		s.files = slices.Delete(s.files, i, i+1)
	}
}

func (s *spool) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.files)
}

func (s *spool) stats() (int, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.files), s.total
}
