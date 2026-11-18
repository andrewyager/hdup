package balance

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"
)

// testBalancer records each dialled address and fails the addresses in fail.
func testBalancer(addrs []string, fail map[string]bool) (*Balancer, *[]string, *time.Time) {
	b := New(addrs)
	var dialled []string
	now := time.Unix(1000, 0)
	b.now = func() time.Time { return now }
	b.dial = func(_ context.Context, _, addr string) (net.Conn, error) {
		dialled = append(dialled, addr)
		host, _, _ := net.SplitHostPort(addr)
		if fail[host] {
			return nil, errors.New("refused")
		}
		c, _ := net.Pipe()
		return c, nil
	}
	return b, &dialled, &now
}

func dial(t *testing.T, b *Balancer) net.Conn {
	t.Helper()
	c, _ := b.DialContext(context.Background(), "tcp", "s3.example.com:443")
	return c
}

func TestDialRotatesAcrossAllAddresses(t *testing.T) {
	b, dialled, _ := testBalancer([]string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}, nil)
	for range 6 {
		dial(t, b)
	}
	want := []string{"10.0.0.1:443", "10.0.0.2:443", "10.0.0.3:443", "10.0.0.1:443", "10.0.0.2:443", "10.0.0.3:443"}
	if !reflect.DeepEqual(*dialled, want) {
		t.Fatalf("got %v", *dialled)
	}
}

func TestFailedAddressIsHeldBackThenComesBack(t *testing.T) {
	fail := map[string]bool{"10.0.0.2": true}
	b, dialled, now := testBalancer([]string{"10.0.0.1", "10.0.0.2"}, fail)
	dial(t, b) // .1
	dial(t, b) // .2 fails
	dial(t, b) // .1
	dial(t, b) // .2 is held back, so .1
	want := []string{"10.0.0.1:443", "10.0.0.2:443", "10.0.0.1:443", "10.0.0.1:443"}
	if !reflect.DeepEqual(*dialled, want) {
		t.Fatalf("during the hold: got %v", *dialled)
	}
	delete(fail, "10.0.0.2")
	*now = now.Add(b.Hold + time.Second)
	*dialled = nil
	dial(t, b)
	dial(t, b)
	if !reflect.DeepEqual(*dialled, []string{"10.0.0.2:443", "10.0.0.1:443"}) {
		t.Fatalf("after the hold: got %v", *dialled)
	}
}

func TestDialStillTriesWhenAllAddressesAreHeldBack(t *testing.T) {
	b, dialled, _ := testBalancer([]string{"10.0.0.1"}, map[string]bool{"10.0.0.1": true})
	dial(t, b)
	dial(t, b)
	if len(*dialled) != 2 {
		t.Fatalf("got %d dials, want 2", len(*dialled))
	}
}

func TestCountsShowsOpenConnectionsForEachAddress(t *testing.T) {
	b, _, _ := testBalancer([]string{"10.0.0.1", "10.0.0.2"}, nil)
	c1 := dial(t, b)
	dial(t, b)
	dial(t, b)
	if got := b.Counts(); got["10.0.0.1"] != 2 || got["10.0.0.2"] != 1 {
		t.Fatalf("got %v", got)
	}
	c1.Close()
	c1.Close() // a second Close must not count again
	if got := b.Counts(); got["10.0.0.1"] != 1 {
		t.Fatalf("after Close: got %v", got)
	}
}

func TestClientKeepsHTTP2Off(t *testing.T) {
	b := New([]string{"10.0.0.1"})
	tr := b.transport(4)
	if tr.ForceAttemptHTTP2 || tr.TLSNextProto == nil || tr.MaxIdleConnsPerHost != 4 {
		t.Fatalf("transport %+v", tr)
	}
}

func TestDialThatACancelDidStopDoesNotHoldTheAddressBack(t *testing.T) {
	b, dialled, _ := testBalancer([]string{"10.0.0.1", "10.0.0.2"}, nil)
	dial := b.dial
	b.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if err := ctx.Err(); err != nil {
			*dialled = append(*dialled, addr)
			return nil, err
		}
		return dial(ctx, network, addr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.DialContext(ctx, "tcp", "s3.example.com:443"); err == nil {
		t.Fatal("got no error from a cancelled dial")
	}
	dial2 := func() { b.DialContext(context.Background(), "tcp", "s3.example.com:443") }
	dial2() // .2
	dial2() // .1 again, because it is not held back
	want := []string{"10.0.0.1:443", "10.0.0.2:443", "10.0.0.1:443"}
	if !reflect.DeepEqual(*dialled, want) {
		t.Fatalf("got %v, want %v", *dialled, want)
	}
}
