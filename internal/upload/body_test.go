package upload

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/andrewyager/hdup/internal/job"
)

func TestBodyIsAReadSeekerWithNoWriteTo(t *testing.T) {
	var b any = newBody(make([]byte, 4), nil, time.Hour, func() {})
	if _, ok := b.(io.ReadSeeker); !ok {
		t.Fatal("the body is not an io.ReadSeeker")
	}
	if _, ok := b.(io.WriterTo); ok {
		t.Fatal("the body has WriteTo")
	}
	if _, ok := b.(interface{ Len() int }); ok {
		t.Fatal("the body has Len")
	}
}

func TestBodyCountsReadsAndGivesBackOnASeekBack(t *testing.T) {
	var sent hook
	b := newBody([]byte("0123456789"), sent.add, time.Hour, func() {})
	// The SDK finds the length with a seek to the end and back.
	if n, err := b.Seek(0, io.SeekEnd); n != 10 || err != nil {
		t.Fatalf("seek to end: %d, %v", n, err)
	}
	if n, err := b.Seek(0, io.SeekStart); n != 0 || err != nil {
		t.Fatalf("seek to start: %d, %v", n, err)
	}
	if sent.sum.Load() != 0 || sent.calls.Load() != 0 {
		t.Fatalf("the length check changed the count: %d", sent.sum.Load())
	}
	// A payload hash reads all of the body and seeks back to the start.
	if got, _ := io.ReadAll(b); string(got) != "0123456789" || sent.sum.Load() != 10 {
		t.Fatalf("got %q, count %d", got, sent.sum.Load())
	}
	b.Seek(4, io.SeekStart)
	if sent.sum.Load() != 4 {
		t.Fatalf("after a seek back to 4: count %d", sent.sum.Load())
	}
	if got, _ := io.ReadAll(b); string(got) != "456789" || sent.sum.Load() != 10 {
		t.Fatalf("got %q, count %d", got, sent.sum.Load())
	}
	b.finish(false)
	if sent.sum.Load() != 0 {
		t.Fatalf("after a failed try: count %d", sent.sum.Load())
	}
	if _, err := b.Read(make([]byte, 4)); err == nil || sent.sum.Load() != 0 {
		t.Fatalf("a read after finish got %v, count %d", err, sent.sum.Load())
	}
}

// TestSentIsThePartSizeThroughTheRealSDK sends one part with the real S3
// client. Over HTTPS the SDK sends an unsigned payload; over HTTP it reads
// the body for the payload hash and seeks back to the start.
func TestSentIsThePartSizeThroughTheRealSDK(t *testing.T) {
	for _, tc := range []struct {
		name, hash string
		start      func(http.Handler) *httptest.Server
	}{
		{"https", "UNSIGNED-PAYLOAD", httptest.NewTLSServer},
		{"http", "", httptest.NewServer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hashHeader atomic.Value
			var got atomic.Int64
			srv := tc.start(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hashHeader.Store(r.Header.Get("X-Amz-Content-Sha256"))
				n, _ := io.Copy(io.Discard, r.Body)
				got.Store(n)
				w.Header().Set("ETag", `"etag"`)
			}))
			defer srv.Close()
			remote := job.S3Remote{Endpoint: strings.TrimPrefix(srv.URL, "https://"), Region: "us-east-1", AccessKey: "AK", SecretKey: "SK"}
			client := NewClient(remote, srv.Client())
			if tc.name == "http" {
				client = s3.New(client.Options(), func(o *s3.Options) { o.BaseEndpoint = aws.String(srv.URL) })
			}
			buf := bytes.Repeat([]byte("x"), 1<<20)
			var sent hook
			u := &Uploader{Sent: sent.add, IdleTimeout: 5 * time.Second}
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
			if got.Load() != int64(len(buf)) {
				t.Fatalf("the server got %d bytes", got.Load())
			}
			h, _ := hashHeader.Load().(string)
			if tc.hash != "" && h != tc.hash || tc.hash == "" && len(h) != 64 {
				t.Fatalf("payload hash header %q", h)
			}
			if sent.sum.Load() != int64(len(buf)) || sent.calls.Load() < 2 {
				t.Fatalf("Sent hook: sum %d, %d calls", sent.sum.Load(), sent.calls.Load())
			}
		})
	}
}
