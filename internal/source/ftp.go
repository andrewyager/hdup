package source

import (
	"context"
	"fmt"
	"io"
	"net"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/jlaffaye/ftp"
)

// FTP is a Source for a HyperDeck. Each call uses its own connection, so
// calls can run in parallel.
type FTP struct {
	Addr string // host:port
	User string
	Pass string
	Root string // directory on the deck, for example "internal/"
}

// idleTimeout is the longest time that one read or write on a deck may wait.
var idleTimeout = 60 * time.Second

// idleConn sets an idle deadline before each read and write.
type idleConn struct{ net.Conn }

func (c idleConn) Read(b []byte) (int, error) {
	c.SetDeadline(time.Now().Add(idleTimeout))
	return c.Conn.Read(b)
}

func (c idleConn) Write(b []byte) (int, error) {
	c.SetDeadline(time.Now().Add(idleTimeout))
	return c.Conn.Write(b)
}

// session holds the connections of one call, so that they close together.
type session struct {
	mu     sync.Mutex
	conns  []net.Conn
	closed bool
}

func (s *session) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: 30 * time.Second}
	c, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		c.Close()
		return nil, context.Canceled
	}
	s.conns = append(s.conns, c)
	return idleConn{c}, nil
}

func (s *session) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for _, c := range s.conns {
		c.Close()
	}
}

// connect opens a logged-in connection. The caller must call done when it
// ends; done(true) sends QUIT before it closes the connections. A call that
// waits on the deck ends when ctx ends.
func (f *FTP) connect(ctx context.Context) (c *ftp.ServerConn, done func(quit bool), err error) {
	s := &session{}
	stop := context.AfterFunc(ctx, s.closeAll)
	done = func(quit bool) {
		stop()
		if quit && c != nil {
			c.Quit()
		}
		s.closeAll()
	}
	// The decks have no MLSD. Always use LIST.
	c, err = ftp.Dial(f.Addr,
		ftp.DialWithDialFunc(func(network, addr string) (net.Conn, error) {
			return s.dial(ctx, network, addr)
		}),
		ftp.DialWithDisabledMLSD(true))
	if err == nil {
		err = c.Login(f.User, f.Pass)
	}
	if err != nil {
		done(true)
		return nil, nil, err
	}
	return c, done, nil
}

// validName is false for a name that could leave its directory.
func validName(name string) bool {
	return !strings.ContainsAny(name, "/\x00")
}

func (f *FTP) List(ctx context.Context) ([]File, error) {
	c, done, err := f.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer done(true)
	var out []File
	var walk func(rel string) error
	walk = func(rel string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := c.List(path.Join(f.Root, rel))
		if err != nil {
			return fmt.Errorf("list %s: %w", path.Join(f.Root, rel), err)
		}
		for _, e := range entries {
			if e.Name == "." || e.Name == ".." {
				continue
			}
			if !validName(e.Name) {
				return fmt.Errorf("list %s: invalid entry name %q", path.Join(f.Root, rel), e.Name)
			}
			p := path.Join(rel, e.Name)
			switch e.Type {
			case ftp.EntryTypeFolder:
				if err := walk(p); err != nil {
					return err
				}
			case ftp.EntryTypeFile:
				// LIST gives the time to the minute only. MDTM is accurate.
				mt := e.Time
				if c.IsGetTimeSupported() {
					if mt, err = c.GetTime(path.Join(f.Root, p)); err != nil {
						return fmt.Errorf("time of %s: %w", p, err)
					}
				}
				out = append(out, File{Path: p, Size: int64(e.Size), ModTime: mt})
			}
		}
		return nil
	}
	if err := walk(""); err != nil {
		return nil, err
	}
	return out, nil
}

func (f *FTP) Size(ctx context.Context, p string) (int64, error) {
	c, done, err := f.connect(ctx)
	if err != nil {
		return 0, err
	}
	defer done(true)
	return c.FileSize(path.Join(f.Root, p))
}

// progressChunk is the number of bytes between two progress calls.
const progressChunk = 256 << 10

func (f *FTP) ReadRange(ctx context.Context, p string, offset int64, buf []byte) error {
	return f.ReadRangeProgress(ctx, p, offset, buf, func(int64) {})
}

func (f *FTP) ReadRangeProgress(ctx context.Context, p string, offset int64, buf []byte, progress func(n int64)) error {
	c, done, err := f.connect(ctx)
	if err != nil {
		return err
	}
	// RETR sends the file to its end, and a deck can be slow to send the
	// transfer reply. Close the connections directly: no reply and no QUIT.
	defer done(false)
	r, err := c.RetrFrom(path.Join(f.Root, p), uint64(offset))
	if err != nil {
		return fmt.Errorf("read %s at %d: %w", p, offset, err)
	}
	for off := 0; off < len(buf); {
		n := min(progressChunk, len(buf)-off)
		if _, err := io.ReadFull(r, buf[off:off+n]); err != nil {
			return fmt.Errorf("read %s at %d: %w", p, offset+int64(off), err)
		}
		progress(int64(n))
		off += n
	}
	return nil
}
