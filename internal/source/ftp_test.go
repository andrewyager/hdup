package source

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	ftpserver "github.com/fclairamb/ftpserverlib"
	"github.com/spf13/afero"
)

type ftpDriver struct{ fs afero.Fs }

func (d *ftpDriver) GetSettings() (*ftpserver.Settings, error) {
	return &ftpserver.Settings{ListenAddr: "127.0.0.1:0", DisableMLSD: true, DisableMLST: true}, nil
}
func (d *ftpDriver) ClientConnected(ftpserver.ClientContext) (string, error) { return "test", nil }
func (d *ftpDriver) ClientDisconnected(ftpserver.ClientContext)              {}
func (d *ftpDriver) AuthUser(ftpserver.ClientContext, string, string) (ftpserver.ClientDriver, error) {
	return d.fs, nil
}
func (d *ftpDriver) GetTLSConfig() (*tls.Config, error) { return nil, nil }

var testTime = time.Date(2026, 11, 12, 3, 4, 5, 0, time.UTC)

// startFTP serves files (path to content) and returns a Source with root "internal/".
func startFTP(t *testing.T, files map[string]string) *FTP {
	t.Helper()
	return startFTPAt(t, files, testTime)
}

// startFTPAt is startFTP with a given modified time for the files.
func startFTPAt(t *testing.T, files map[string]string, modTime time.Time) *FTP {
	t.Helper()
	fs := afero.NewMemMapFs()
	for p, content := range files {
		if err := afero.WriteFile(fs, p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := fs.Chtimes(p, modTime, modTime); err != nil {
			t.Fatal(err)
		}
	}
	srv := ftpserver.NewFtpServer(&ftpDriver{fs})
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Stop() })
	return &FTP{Addr: srv.Addr(), User: "anonymous", Pass: "anonymous", Root: "internal/"}
}

func TestFTPListWalksDirectoriesAndUsesTheAccurateTime(t *testing.T) {
	src := startFTP(t, map[string]string{
		"/internal/top.mov":       "12345",
		"/internal/CH1/a.mov":     "0123456789",
		"/internal/CH1/sub/b.mov": "xy",
		"/elsewhere/c.mov":        "no",
	})
	got, err := src.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got = Select(got, testTime.Add(-time.Hour), testTime.Add(time.Hour), nil)
	want := []File{
		{Path: "CH1/a.mov", Size: 10, ModTime: testTime},
		{Path: "CH1/sub/b.mov", Size: 2, ModTime: testTime},
		{Path: "top.mov", Size: 5, ModTime: testTime},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestFTPReadRangeReadsFromTheOffset(t *testing.T) {
	src := startFTP(t, map[string]string{"/internal/CH1/a.mov": "0123456789"})
	buf := make([]byte, 3)
	if err := src.ReadRange(context.Background(), "CH1/a.mov", 4, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "456" {
		t.Fatalf("got %q", buf)
	}
}

func TestFTPReadRangeFailsWhenTheFileIsTooShort(t *testing.T) {
	src := startFTP(t, map[string]string{"/internal/a.mov": "0123456789"})
	if err := src.ReadRange(context.Background(), "a.mov", 8, make([]byte, 5)); err == nil {
		t.Fatal("got no error for a read past the end of the file")
	}
}

func TestFTPSizeReturnsTheFileSize(t *testing.T) {
	src := startFTP(t, map[string]string{"/internal/a.mov": "0123456789"})
	got, err := src.Size(context.Background(), "a.mov")
	if err != nil || got != 10 {
		t.Fatalf("got %d, %v", got, err)
	}
}

// conns keeps the server side of test connections, to close them in one cleanup.
type conns struct {
	mu   sync.Mutex
	list []net.Conn
}

func (c *conns) add(conn net.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.list = append(c.list, conn)
}

func (c *conns) closeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, conn := range c.list {
		conn.Close()
	}
}

// listen returns a listener and a set of connections that the test cleanup closes.
func listen(t *testing.T) (net.Listener, *conns) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	open := &conns{}
	t.Cleanup(func() {
		l.Close()
		open.closeAll()
	})
	return l, open
}

// startSilentDeck accepts connections and never sends data.
func startSilentDeck(t *testing.T) *FTP {
	t.Helper()
	l, open := listen(t)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			open.add(c)
		}
	}()
	return &FTP{Addr: l.Addr().String(), User: "u", Pass: "p", Root: "internal/"}
}

// startSlowReplyDeck serves content for RETR, but then keeps the data
// connection open and never sends the transfer reply.
func startSlowReplyDeck(t *testing.T, content string) *FTP {
	t.Helper()
	l, open := listen(t)
	serve := func(c net.Conn) {
		r := bufio.NewReader(c)
		fmt.Fprint(c, "220 ready\r\n")
		var offset int
		var data chan net.Conn
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd, arg, _ := strings.Cut(strings.TrimSpace(line), " ")
			switch cmd {
			case "USER":
				fmt.Fprint(c, "331 password\r\n")
			case "PASS":
				fmt.Fprint(c, "230 logged in\r\n")
			case "TYPE":
				fmt.Fprint(c, "200 ok\r\n")
			case "EPSV":
				dl, dopen := listen(t)
				data = make(chan net.Conn, 1)
				go func() {
					if d, err := dl.Accept(); err == nil {
						dopen.add(d)
						data <- d
					}
				}()
				fmt.Fprintf(c, "229 passive (|||%d|)\r\n", dl.Addr().(*net.TCPAddr).Port)
			case "REST":
				offset, _ = strconv.Atoi(arg)
				fmt.Fprint(c, "350 ok\r\n")
			case "RETR":
				fmt.Fprint(c, "150 sending\r\n")
				(<-data).Write([]byte(content[offset:]))
			default:
				fmt.Fprint(c, "502 no\r\n")
			}
		}
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			open.add(c)
			go serve(c)
		}
	}()
	return &FTP{Addr: l.Addr().String(), User: "u", Pass: "p", Root: "internal/"}
}

func TestFTPReadRangeReturnsWithoutTheTransferReply(t *testing.T) {
	setIdleTimeout(t, time.Minute)
	src := startSlowReplyDeck(t, "0123456789")
	done := make(chan error, 1)
	buf := make([]byte, 3)
	go func() { done <- src.ReadRange(context.Background(), "a.mov", 4, buf) }()
	select {
	case err := <-done:
		if err != nil || string(buf) != "456" {
			t.Fatalf("got %q, %v", buf, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadRange waited for the transfer reply")
	}
}

func TestFTPReadRangeProgressReportsEachChunk(t *testing.T) {
	content := make([]byte, 700<<10)
	for i := range content {
		content[i] = byte(i % 251)
	}
	src := startFTP(t, map[string]string{"/internal/a.mov": string(content)})
	var _ ProgressReader = src
	buf := make([]byte, 600<<10)
	var calls []int64
	var sum int64
	err := src.ReadRangeProgress(context.Background(), "a.mov", 1000, buf, func(n int64) {
		calls = append(calls, n)
		sum += n
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, content[1000:1000+len(buf)]) {
		t.Fatal("the range content is not correct")
	}
	if sum != int64(len(buf)) || len(calls) != 3 {
		t.Fatalf("got calls %v, sum %d", calls, sum)
	}
	for _, n := range calls {
		if n > 256<<10 {
			t.Fatalf("got a call of %d bytes", n)
		}
	}
}

func setIdleTimeout(t *testing.T, d time.Duration) {
	old := idleTimeout
	idleTimeout = d
	t.Cleanup(func() { idleTimeout = old })
}

func TestFTPListFailsWhenTheDeckStaysSilentForTheIdleTimeout(t *testing.T) {
	setIdleTimeout(t, 200*time.Millisecond)
	src := startSilentDeck(t)
	start := time.Now()
	if _, err := src.List(context.Background()); err == nil {
		t.Fatal("got no error from a silent deck")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %s", d)
	}
}

func TestFTPSizeReturnsWhenTheContextEndsAndTheDeckIsSilent(t *testing.T) {
	setIdleTimeout(t, time.Minute)
	src := startSilentDeck(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := src.Size(ctx, "a.mov"); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("got no error from a silent deck")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Size did not return after the context ended")
	}
}

func TestValidNameRejectsSlashAndNul(t *testing.T) {
	for name, want := range map[string]bool{"a.mov": true, "a b": true, "../x": false, "a/b": false, "a\x00b": false} {
		if got := validName(name); got != want {
			t.Errorf("validName(%q) = %v", name, got)
		}
	}
}

func TestFTPStatGivesSizeAndTimeOfEachPathAndAnErrorForAMissingFile(t *testing.T) {
	src := startFTP(t, map[string]string{"/internal/CH1/a.mov": "0123456789", "/internal/b.mov": "xy"})
	src.Root = ""
	got, err := src.Stat(context.Background(), []string{"internal/CH1/a.mov", "internal/nope.mov", "internal/b.mov"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (File{Path: "internal/CH1/a.mov", Size: 10, ModTime: testTime}); got[0].Err != nil || got[0].File != want {
		t.Fatalf("got %+v", got[0])
	}
	if got[1].Err == nil {
		t.Fatal("a missing file gave no error")
	}
	if want := (File{Path: "internal/b.mov", Size: 2, ModTime: testTime}); got[2].Err != nil || got[2].File != want {
		t.Fatalf("got %+v", got[2])
	}
}

func TestFTPStatFailsAsAWholeWhenTheDeckDoesNotAnswer(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	src := &FTP{Addr: addr, User: "anonymous", Pass: "anonymous"}
	if _, err := src.Stat(context.Background(), []string{"a.mov"}); err == nil {
		t.Fatal("no error for a deck that does not answer")
	}
}

// withoutMDTM puts a proxy before the deck that removes MDTM from the FEAT
// reply. A HyperDeck has no MDTM.
func withoutMDTM(t *testing.T, src *FTP) *FTP {
	t.Helper()
	l, open := listen(t)
	go func() {
		for {
			client, err := l.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", src.Addr)
			if err != nil {
				client.Close()
				return
			}
			open.add(client)
			open.add(server)
			go func() {
				buf := make([]byte, 4096)
				for {
					n, err := client.Read(buf)
					if n > 0 {
						server.Write(buf[:n])
					}
					if err != nil {
						server.Close()
						return
					}
				}
			}()
			go func() {
				r := bufio.NewReader(server)
				for {
					line, err := r.ReadString('\n')
					if strings.TrimSpace(line) != "MDTM" {
						client.Write([]byte(line))
					}
					if err != nil {
						client.Close()
						return
					}
				}
			}()
		}
	}()
	out := *src
	out.Addr = l.Addr().String()
	return &out
}

func TestFTPStatUsesTheTimeFromTheListWhenTheDeckHasNoMDTM(t *testing.T) {
	// The list gives the time to the minute, and only for a recent file.
	minute := time.Now().Add(-time.Hour).UTC().Truncate(time.Minute)
	src := startFTPAt(t, map[string]string{"/internal/CH 1/a.mov": "0123456789", "/internal/b.mov": "xy"}, minute.Add(5*time.Second))
	src.Root = ""
	src = withoutMDTM(t, src)
	got, err := src.Stat(context.Background(), []string{"internal/CH 1/a.mov", "internal/nope.mov", "internal/CH 9/c.mov", "internal/b.mov"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (File{Path: "internal/CH 1/a.mov", Size: 10, ModTime: minute}); got[0].Err != nil || got[0].File != want {
		t.Fatalf("got %+v", got[0])
	}
	if got[1].Err == nil || got[2].Err == nil {
		t.Fatalf("a missing file gave no error: %+v, %+v", got[1], got[2])
	}
	if want := (File{Path: "internal/b.mov", Size: 2, ModTime: minute}); got[3].Err != nil || got[3].File != want {
		t.Fatalf("got %+v", got[3])
	}
}
