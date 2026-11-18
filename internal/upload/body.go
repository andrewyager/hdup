package upload

import (
	"errors"
	"io"
	"sync"
	"time"
)

var errTryEnded = errors.New("the send try has ended")

// body is the request body of one send try. It tells the Sent hook about
// each byte that the HTTP client reads, and each Read restarts the idle
// timer. It has no WriteTo and no Len: with WriteTo, the HTTP client copies
// the body in one step and the count jumps.
type body struct {
	mu      sync.Mutex
	buf     []byte
	pos     int64 // the read position
	counted int64 // the bytes that the hook got
	ended   bool
	sent    func(int64)
	idle    time.Duration
	timer   *time.Timer
}

// newBody starts the idle timer. onIdle runs when no Read occurs for idle.
func newBody(buf []byte, sent func(int64), idle time.Duration, onIdle func()) *body {
	return &body{buf: buf, sent: sent, idle: idle, timer: time.AfterFunc(idle, onIdle)}
}

func (b *body) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ended {
		return 0, errTryEnded
	}
	if b.pos >= int64(len(b.buf)) {
		return 0, io.EOF
	}
	n := copy(p, b.buf[b.pos:])
	b.pos += int64(n)
	b.timer.Reset(b.idle)
	if b.pos > b.counted {
		call(b.sent, b.pos-b.counted)
		b.counted = b.pos
	}
	return n, nil
}

// Seek gives back the counted bytes after the new position. A seek to the end
// and back, which finds the length, does not change the count.
func (b *body) Seek(offset int64, whence int) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ended {
		return 0, errTryEnded
	}
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += b.pos
	case io.SeekEnd:
		offset += int64(len(b.buf))
	default:
		return 0, errors.New("seek: incorrect whence")
	}
	if offset < 0 {
		return 0, errors.New("seek: negative position")
	}
	b.pos = offset
	if b.pos < b.counted {
		call(b.sent, b.pos-b.counted)
		b.counted = b.pos
	}
	return b.pos, nil
}

// finish ends the try and stops the timer. After a failed try, the hook gets
// back all counted bytes. After a good try, the count becomes the body size.
// The HTTP client can read the body after the request returns; such a Read
// gets an error and does not touch buf.
func (b *body) finish(ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ended = true
	b.timer.Stop()
	end := int64(0)
	if ok {
		end = int64(len(b.buf))
	}
	if end != b.counted {
		call(b.sent, end-b.counted)
		b.counted = end
	}
}
