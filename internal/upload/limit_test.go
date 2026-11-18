package upload

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/andrewyager/hdup/internal/job"
)

// fakeClock is a clock that moves only when a limiter sleeps.
type fakeClock struct{ t time.Time }

func (c *fakeClock) limiter(rate int64) *Limiter {
	l := NewLimiter(rate)
	l.now = func() time.Time { return c.t }
	l.sleep = func(d time.Duration) {
		if d > 0 {
			c.t = c.t.Add(d)
		}
	}
	return l
}

func TestLimiterGivesPiecesAtTheRate(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	l := c.limiter(64 << 10)
	start, total := c.t, 0
	for total < 640<<10 {
		n := l.wait(1 << 20)
		if n < 1 || n > 64<<10 {
			t.Fatalf("piece of %d bytes", n)
		}
		total += n
	}
	// The first piece has no delay, so the time is a little less than 10 s.
	if d := c.t.Sub(start); d < 9*time.Second || d > 10*time.Second {
		t.Fatalf("640 KiB at 64 KiB/s took %s", d)
	}
}

func TestLimiterDoesNotSaveTimeThatWasNotUsed(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	l := c.limiter(1 << 10)
	l.wait(1 << 10)
	c.t = c.t.Add(time.Hour)
	start := c.t
	for range 4 {
		l.wait(1 << 10)
	}
	if d := c.t.Sub(start); d < 2*time.Second {
		t.Fatalf("4 KiB at 1 KiB/s after an idle hour took %s", d)
	}
}

func TestNilLimiterGivesAllBytesWithNoDelay(t *testing.T) {
	var l *Limiter
	if n := l.wait(1 << 20); n != 1<<20 {
		t.Fatalf("got %d", n)
	}
}

func TestBodyWithALimiterGivesTheDataInPiecesAndCountsEachPiece(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	var sent hook
	data := make([]byte, 8<<10)
	for i := range data {
		data[i] = byte(i)
	}
	b := newBody(data, sent.add, time.Hour, func() {}, c.limiter(1<<10))
	start := c.t
	got, err := io.ReadAll(b)
	if err != nil || string(got) != string(data) {
		t.Fatalf("read %d bytes, %v", len(got), err)
	}
	if d := c.t.Sub(start); d < 6*time.Second || d > 9*time.Second {
		t.Fatalf("8 KiB at 1 KiB/s took %s", d)
	}
	if sent.sum.Load() != 8<<10 {
		t.Fatalf("counted %d", sent.sum.Load())
	}
}

func TestSendWithALimitThroughTheRealSDKTakesTheTimeOfTheRate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("ETag", `"etag"`)
	}))
	defer srv.Close()
	remote := job.S3Remote{Endpoint: strings.TrimPrefix(srv.URL, "https://"), Region: "us-east-1", AccessKey: "AK", SecretKey: "SK"}
	client := NewClient(remote, srv.Client())
	buf := bytes.Repeat([]byte("x"), 256<<10)
	u := &Uploader{IdleTimeout: 5 * time.Second, Limit: NewLimiter(1 << 20)}
	start := time.Now()
	err := u.send(context.Background(), buf, func(ctx context.Context, body io.ReadSeeker) error {
		_, err := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String("b"), Key: aws.String("k"), UploadId: aws.String("u"), PartNumber: aws.Int32(1),
			Body: body, ContentLength: aws.Int64(int64(len(buf))), ContentMD5: contentMD5(buf),
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// 256 KiB at 1 MiB/s is 250 ms.
	if d := time.Since(start); d < 150*time.Millisecond || d > 5*time.Second {
		t.Fatalf("took %s", d)
	}
}
