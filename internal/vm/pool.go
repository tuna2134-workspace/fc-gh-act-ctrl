package vm

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// clonePool keeps ready rootfs copies so Start does not pay the copy cost on
// the critical path. It fills lazily and refills after every take.
type clonePool struct {
	dir     string
	base    string
	size    int
	reflink bool
	log     *slog.Logger

	mu      sync.Mutex
	files   []string
	started bool
	closed  bool
	wake    chan struct{}
	stop    chan struct{}
	wg      sync.WaitGroup
}

func newClonePool(dir, base string, size int, reflink bool, log *slog.Logger) *clonePool {
	if log == nil {
		log = slog.Default()
	}
	return &clonePool{
		dir:     dir,
		base:    base,
		size:    size,
		reflink: reflink,
		log:     log,
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
	}
}

func (p *clonePool) start() {
	p.mu.Lock()
	if p.started || p.closed || p.size <= 0 {
		p.mu.Unlock()
		return
	}
	p.started = true
	if err := os.MkdirAll(p.dir, 0o750); err != nil {
		p.started = false
		p.mu.Unlock()
		p.log.Warn("create preclone pool", "dir", p.dir, "error", err)
		return
	}
	// Remove clones left over by a previous controller process.
	if entries, err := os.ReadDir(p.dir); err == nil {
		for _, entry := range entries {
			_ = os.RemoveAll(filepath.Join(p.dir, entry.Name()))
		}
	}
	p.wg.Add(1)
	p.mu.Unlock()

	go p.fill()
}

// take removes and returns one ready clone, or "" when the pool is empty.
func (p *clonePool) take() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.files) == 0 {
		return ""
	}
	path := p.files[len(p.files)-1]
	p.files = p.files[:len(p.files)-1]
	select {
	case p.wake <- struct{}{}:
	default:
	}
	return path
}

func (p *clonePool) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	started := p.started
	p.mu.Unlock()

	if started {
		close(p.stop)
		p.wg.Wait()
	}
	_ = os.RemoveAll(p.dir)
}

func (p *clonePool) fill() {
	defer p.wg.Done()
	for {
		p.mu.Lock()
		n := len(p.files)
		closed := p.closed
		p.mu.Unlock()
		if closed {
			return
		}
		if n >= p.size {
			select {
			case <-p.wake:
			case <-p.stop:
				return
			}
			continue
		}

		path := filepath.Join(p.dir, uuid.NewString()+".ext4")
		if _, err := cloneFile(p.base, path, p.reflink); err != nil {
			p.log.Warn("preclone rootfs", "error", err)
			select {
			case <-time.After(2 * time.Second):
			case <-p.stop:
				_ = os.Remove(path)
				return
			}
			continue
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = os.Remove(path)
			return
		}
		p.files = append(p.files, path)
		p.mu.Unlock()
		p.log.Info("precloned rootfs for the pool", "path", path)
	}
}
