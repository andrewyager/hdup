// Package balance spreads new connections across the addresses of one host.
package balance

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

type Balancer struct {
	Hold time.Duration // time that a failed address is held back

	mu     sync.Mutex
	addrs  []string
	next   int
	held   map[string]time.Time
	active map[string]int
	now    func() time.Time
	dial   func(ctx context.Context, network, addr string) (net.Conn, error)
}

func New(addrs []string) *Balancer {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &Balancer{
		Hold:   30 * time.Second,
		addrs:  addrs,
		held:   map[string]time.Time{},
		active: map[string]int{},
		now:    time.Now,
		dial:   d.DialContext,
	}
}

// Resolve returns the IPv4 addresses of host in sorted order.
func Resolve(ctx context.Context, host string) ([]string, error) {
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%s has no IPv4 address", host)
	}
	out := make([]string, len(ips))
	for i, ip := range ips {
		out[i] = ip.String()
	}
	sort.Strings(out)
	return out, nil
}

// pick returns the next address that is not held back. If all addresses are
// held back, it returns the next address.
func (b *Balancer) pick() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	for range b.addrs {
		a := b.addrs[b.next%len(b.addrs)]
		b.next++
		if !now.Before(b.held[a]) {
			return a
		}
	}
	a := b.addrs[b.next%len(b.addrs)]
	b.next++
	return a
}

// DialContext ignores the host in addr and connects to the next address.
func (b *Balancer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ip := b.pick()
	c, err := b.dial(ctx, network, net.JoinHostPort(ip, port))
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		// A cancelled dial says nothing about the address.
		if ctx.Err() == nil {
			b.held[ip] = b.now().Add(b.Hold)
		}
		return nil, err
	}
	b.active[ip]++
	return &conn{Conn: c, release: func() {
		b.mu.Lock()
		b.active[ip]--
		b.mu.Unlock()
	}}, nil
}

type conn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *conn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}

func (b *Balancer) Counts() map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]int, len(b.addrs))
	for _, a := range b.addrs {
		out[a] = b.active[a]
	}
	return out
}

func (b *Balancer) transport(maxConns int) *http.Transport {
	return &http.Transport{
		DialContext:         b.DialContext,
		MaxIdleConns:        maxConns,
		MaxIdleConnsPerHost: maxConns,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		// An empty map keeps HTTP/2 off. HTTP/2 uses one connection for all requests.
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
}

// Client returns an HTTP client that keeps up to maxConns connections open.
func (b *Balancer) Client(maxConns int) *http.Client {
	return &http.Client{Transport: b.transport(maxConns)}
}
