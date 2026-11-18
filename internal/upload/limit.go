package upload

import (
	"sync"
	"time"
)

// Limiter limits the total send rate of all the bodies that use it. It gives
// each caller a small piece in turn, so that each connection moves data
// continuously.
type Limiter struct {
	mu    sync.Mutex
	rate  float64   // bytes for each second
	piece int       // the largest number of bytes for one wait
	next  time.Time // the time at which the next piece can go
	now   func() time.Time
	sleep func(time.Duration)
}

// NewLimiter makes a Limiter for a rate in bytes for each second.
func NewLimiter(bytesPerSecond int64) *Limiter {
	return &Limiter{
		rate:  float64(bytesPerSecond),
		piece: int(min(max(bytesPerSecond/64, 1<<10), 64<<10)),
		now:   time.Now,
		sleep: time.Sleep,
	}
}

// wait gives the number of bytes, not more than n, that the caller can send
// now. It sleeps until the time of those bytes. A nil Limiter gives n with no
// delay.
func (l *Limiter) wait(n int) int {
	if l == nil {
		return n
	}
	n = min(n, l.piece)
	l.mu.Lock()
	now := l.now()
	// Time that was not used is not saved: there is no burst.
	if l.next.Before(now) {
		l.next = now
	}
	at := l.next
	l.next = at.Add(time.Duration(float64(n) / l.rate * float64(time.Second)))
	l.mu.Unlock()
	l.sleep(at.Sub(now))
	return n
}
