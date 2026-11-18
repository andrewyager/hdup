package upload

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/andrewyager/hdup/internal/fake"
	"github.com/andrewyager/hdup/internal/source"
)

var data = []byte("0123456789abcd") // 14 bytes: parts of 4, 4, 4 and 2

// setup returns an uploader with 4-byte parts, the S3 substitute, a source
// that holds data as "a.mov", and the File for it.
func setup() (*Uploader, *fake.S3, *fake.Source, source.File) {
	s3 := fake.NewS3()
	src := &fake.Source{Files: map[string][]byte{"a.mov": data}, ModTime: time.Now().Add(-time.Hour)}
	u := &Uploader{API: s3, Bucket: "b", PartSize: 4, Workers: 2, Retries: 3,
		Backoff: time.Millisecond, IdleTimeout: 5 * time.Second}
	return u, s3, src, source.File{Path: "a.mov", Size: int64(len(data)), ModTime: src.ModTime}
}

func failPart(n int32) func(int32) error {
	return func(part int32) error {
		if part == n {
			return errors.New("injected failure")
		}
		return nil
	}
}

func TestUploadSendsALargeFileInPartsAndTheObjectEqualsTheSource(t *testing.T) {
	u, s3, src, f := setup()
	var read, sent int64
	u.Workers = 1 // one worker, so the hooks need no lock
	u.Read = func(n int64) { read += n }
	u.Sent = func(n int64) { sent += n }
	res, err := u.Upload(context.Background(), src, f, "deck/a.mov")
	if err != nil || res != Uploaded {
		t.Fatalf("got %v, %v", res, err)
	}
	if !bytes.Equal(s3.Objects["deck/a.mov"], data) {
		t.Fatalf("object is %q", s3.Objects["deck/a.mov"])
	}
	if s3.PartCalls != 4 || s3.Open() != 0 {
		t.Fatalf("%d part calls, %d open uploads", s3.PartCalls, s3.Open())
	}
	if read != 14 || sent != 14 {
		t.Fatalf("hooks: read %d, sent %d", read, sent)
	}
}

func TestUploadSendsASmallFileWithOnePut(t *testing.T) {
	u, s3, src, f := setup()
	u.PartSize = 64
	if _, err := u.Upload(context.Background(), src, f, "k"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s3.Objects["k"], data) || s3.PartCalls != 0 {
		t.Fatalf("object %q, %d part calls", s3.Objects["k"], s3.PartCalls)
	}
}

func TestUploadSendsAnEmptyFile(t *testing.T) {
	u, s3, src, _ := setup()
	src.Files["empty"] = nil
	if _, err := u.Upload(context.Background(), src, source.File{Path: "empty", ModTime: src.ModTime}, "k"); err != nil {
		t.Fatal(err)
	}
	if b, ok := s3.Objects["k"]; !ok || len(b) != 0 {
		t.Fatalf("object %q, exists %v", b, ok)
	}
}

func TestUploadSkipsAnObjectThatExistsWithTheSameSize(t *testing.T) {
	u, s3, src, f := setup()
	s3.Objects["k"] = bytes.Repeat([]byte("x"), len(data))
	res, err := u.Upload(context.Background(), src, f, "k")
	if err != nil || res != Skipped || s3.PartCalls != 0 {
		t.Fatalf("got %v, %v, %d part calls", res, err, s3.PartCalls)
	}
}

func TestUploadReplacesAnObjectThatHasADifferentSize(t *testing.T) {
	u, s3, src, f := setup()
	s3.Objects["k"] = []byte("short")
	if _, err := u.Upload(context.Background(), src, f, "k"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s3.Objects["k"], data) {
		t.Fatalf("object is %q", s3.Objects["k"])
	}
}

func TestUploadTriesAFailedPartAgain(t *testing.T) {
	u, s3, src, f := setup()
	failed := false
	s3.FailPart = func(part int32) error {
		if part == 2 && !failed {
			failed = true
			return errors.New("injected failure")
		}
		return nil
	}
	if _, err := u.Upload(context.Background(), src, f, "k"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s3.Objects["k"], data) || s3.PartCalls != 5 {
		t.Fatalf("object %q, %d part calls", s3.Objects["k"], s3.PartCalls)
	}
}

func TestUploadFailsAndKeepsTheMultipartUploadWhenAPartFailsOnEachTry(t *testing.T) {
	u, s3, src, f := setup()
	s3.FailPart = failPart(3)
	_, err := u.Upload(context.Background(), src, f, "k")
	if err == nil || !strings.Contains(err.Error(), "part 3") {
		t.Fatalf("got error %v", err)
	}
	if _, ok := s3.Objects["k"]; ok || s3.Open() != 1 {
		t.Fatalf("object exists %v, %d open uploads", ok, s3.Open())
	}
}

func TestUploadResumeSendsOnlyTheMissingParts(t *testing.T) {
	u, s3, src, f := setup()
	u.Workers = 1 // parts go in sequence, so parts 1 and 2 are stored before part 3 fails
	s3.FailPart = failPart(3)
	if _, err := u.Upload(context.Background(), src, f, "k"); err == nil {
		t.Fatal("the first run must fail")
	}
	s3.FailPart = nil
	s3.PartCalls = 0
	var resumed, read int64
	u.Resumed = func(n int64) { resumed += n }
	u.Read = func(n int64) { read += n }
	cs := &countingSource{Source: src}
	if _, err := u.Upload(context.Background(), cs, f, "k"); err != nil {
		t.Fatal(err)
	}
	if s3.PartCalls != 2 || resumed != 8 {
		t.Fatalf("%d part calls, want 2 (parts 3 and 4); %d bytes resumed, want 8", s3.PartCalls, resumed)
	}
	// Two reads verify parts 1 and 2. Two reads get parts 3 and 4.
	if n := cs.reads.Load(); n != 4 || read != 6 {
		t.Fatalf("%d reads, want 4; Read hook got %d bytes, want 6", n, read)
	}
	if !bytes.Equal(s3.Objects["k"], data) || s3.Open() != 0 {
		t.Fatalf("object %q, %d open uploads", s3.Objects["k"], s3.Open())
	}
}

func TestUploadStartsAgainWhenTheSourceIsNewerThanTheMultipartUpload(t *testing.T) {
	u, s3, src, f := setup()
	u.Workers = 1
	s3.FailPart = failPart(3)
	u.Upload(context.Background(), src, f, "k")
	s3.FailPart = nil
	s3.PartCalls = 0
	f.ModTime = time.Now().Add(time.Hour)
	if _, err := u.Upload(context.Background(), src, f, "k"); err != nil {
		t.Fatal(err)
	}
	if s3.PartCalls != 4 || s3.Open() != 0 {
		t.Fatalf("%d part calls, want 4; %d open uploads, want 0", s3.PartCalls, s3.Open())
	}
}

func TestUploadStartsAgainWhenThePartSizesDoNotAgreeWithThePlan(t *testing.T) {
	u, s3, src, f := setup()
	u.Workers = 1
	s3.FailPart = failPart(3)
	u.Upload(context.Background(), src, f, "k")
	s3.FailPart = nil
	s3.PartCalls = 0
	u.PartSize = 8
	if _, err := u.Upload(context.Background(), src, f, "k"); err != nil {
		t.Fatal(err)
	}
	if s3.PartCalls != 2 || s3.Open() != 0 || !bytes.Equal(s3.Objects["k"], data) {
		t.Fatalf("%d part calls, %d open uploads, object %q", s3.PartCalls, s3.Open(), s3.Objects["k"])
	}
}

func TestUploadFailsBeforeAnyS3CallWhenTheSourceSizeIsNotTheListedSize(t *testing.T) {
	u, s3, src, f := setup()
	src.SizeDelta = 1
	_, err := u.Upload(context.Background(), src, f, "k")
	if err == nil || !strings.Contains(err.Error(), "the source size is 15, the list gave 14; the file is possibly recording") {
		t.Fatalf("got error %v", err)
	}
	if _, ok := s3.Objects["k"]; ok || s3.Open() != 0 || s3.PartCalls != 0 {
		t.Fatal("the upload must not start")
	}
}

// growingSource returns a larger size after the first Size call.
type growingSource struct {
	*fake.Source
	calls atomic.Int32
}

func (s *growingSource) Size(ctx context.Context, path string) (int64, error) {
	n, err := s.Source.Size(ctx, path)
	if s.calls.Add(1) > 1 {
		n++
	}
	return n, err
}

func TestUploadOfASmallFileFailsWhenTheSizeChangesDuringTheRead(t *testing.T) {
	u, s3, src, f := setup()
	u.PartSize = 64
	gs := &growingSource{Source: src}
	_, err := u.Upload(context.Background(), gs, f, "k")
	if err == nil || !strings.Contains(err.Error(), "possibly recording") {
		t.Fatalf("got error %v", err)
	}
	if _, ok := s3.Objects["k"]; ok || gs.calls.Load() != 2 {
		t.Fatalf("object stored or %d size calls", gs.calls.Load())
	}
}

func TestUploadOfALargeFileFailsWhenTheSizeChangesDuringTheUpload(t *testing.T) {
	u, s3, src, f := setup()
	gs := &growingSource{Source: src}
	_, err := u.Upload(context.Background(), gs, f, "k")
	if err == nil || !strings.Contains(err.Error(), "possibly recording") {
		t.Fatalf("got error %v", err)
	}
	if _, ok := s3.Objects["k"]; ok {
		t.Fatal("the object must not be completed")
	}
}

func TestUploadStopsWhenTheContextIsCancelled(t *testing.T) {
	u, s3, src, f := setup()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := u.Upload(ctx, src, f, "k"); err == nil {
		t.Fatal("got no error")
	}
	if _, ok := s3.Objects["k"]; ok {
		t.Fatal("the object must not be completed")
	}
}

// countingSource counts the ReadRange calls.
type countingSource struct {
	*fake.Source
	reads atomic.Int32
}

func (s *countingSource) ReadRange(ctx context.Context, path string, offset int64, buf []byte) error {
	s.reads.Add(1)
	return s.Source.ReadRange(ctx, path, offset, buf)
}

// progressSource reports reads in steps of one byte. The first read at an
// offset in failAt fails after half of the range.
type progressSource struct {
	*fake.Source
	mu     sync.Mutex
	failAt map[int64]bool
}

func (s *progressSource) ReadRangeProgress(ctx context.Context, path string, offset int64, buf []byte, progress func(int64)) error {
	if err := s.Source.ReadRange(ctx, path, offset, buf); err != nil {
		return err
	}
	s.mu.Lock()
	fail := s.failAt[offset]
	delete(s.failAt, offset)
	s.mu.Unlock()
	for i := range buf {
		if fail && i == len(buf)/2 {
			return errors.New("injected read failure")
		}
		progress(1)
	}
	return nil
}

// hook adds the values it gets and counts the calls and the negative calls.
type hook struct{ sum, calls, negative atomic.Int64 }

func (h *hook) add(n int64) {
	h.sum.Add(n)
	h.calls.Add(1)
	if n < 0 {
		h.negative.Add(1)
	}
}

func TestUploadReportsReadBytesWhileTheyMoveAndGivesBackAFailedTry(t *testing.T) {
	u, s3, src, f := setup()
	ps := &progressSource{Source: src, failAt: map[int64]bool{4: true}}
	var read hook
	u.Read = read.add
	if _, err := u.Upload(context.Background(), ps, f, "k"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s3.Objects["k"], data) {
		t.Fatalf("object is %q", s3.Objects["k"])
	}
	if read.sum.Load() != 14 || read.calls.Load() <= 4 || read.negative.Load() != 1 {
		t.Fatalf("Read hook: sum %d, %d calls, %d negative", read.sum.Load(), read.calls.Load(), read.negative.Load())
	}
}

func TestUploadSentEqualsTheFileSizeAfterARetriedPart(t *testing.T) {
	u, s3, src, f := setup()
	big := bytes.Repeat([]byte("0123456789"), 300)
	src.Files["a.mov"] = big
	f.Size = int64(len(big))
	u.PartSize = 1500
	failed := atomic.Bool{}
	s3.FailPart = func(part int32) error {
		if part == 2 && failed.CompareAndSwap(false, true) {
			return errors.New("injected failure")
		}
		return nil
	}
	var sent hook
	u.Sent = sent.add
	if _, err := u.Upload(context.Background(), src, f, "k"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s3.Objects["k"], big) {
		t.Fatal("the object is not the source")
	}
	if sent.sum.Load() != 3000 || sent.calls.Load() <= 3 || sent.negative.Load() != 1 {
		t.Fatalf("Sent hook: sum %d, %d calls, %d negative", sent.sum.Load(), sent.calls.Load(), sent.negative.Load())
	}
}

// slowS3 reads the body of each part one byte at a time with a pause.
type slowS3 struct {
	*fake.S3
	pause time.Duration
}

func (s *slowS3) UploadPart(ctx context.Context, in *s3.UploadPartInput, opts ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	var got []byte
	one := make([]byte, 1)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(s.pause):
		}
		n, err := in.Body.Read(one)
		got = append(got, one[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	in.Body = bytes.NewReader(got)
	return s.S3.UploadPart(ctx, in, opts...)
}

func TestUploadSendThatReadsSlowlyButSteadilyIsNotCancelled(t *testing.T) {
	u, s3, src, f := setup()
	big := bytes.Repeat([]byte("0123456789"), 4)
	src.Files["a.mov"] = big
	f.Size = int64(len(big))
	u.PartSize = 20
	u.Retries = 1
	u.IdleTimeout = 100 * time.Millisecond
	u.API = &slowS3{S3: s3, pause: 10 * time.Millisecond} // each part takes more than 200 ms
	if _, err := u.Upload(context.Background(), src, f, "k"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s3.Objects["k"], big) {
		t.Fatal("the object is not the source")
	}
}

// stuckS3 never reads the body of a part and waits for the end of ctx.
type stuckS3 struct{ *fake.S3 }

func (s *stuckS3) UploadPart(ctx context.Context, _ *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestUploadSendThatMovesNoDataFailsAfterTheIdleTimeout(t *testing.T) {
	u, s3, src, f := setup()
	u.Retries = 1
	u.Workers = 1
	u.IdleTimeout = 50 * time.Millisecond
	u.API = &stuckS3{S3: s3}
	start := time.Now()
	_, err := u.Upload(context.Background(), src, f, "k")
	if err == nil || !strings.Contains(err.Error(), "no data") {
		t.Fatalf("got error %v", err)
	}
	if d := time.Since(start); d < 50*time.Millisecond || d > 5*time.Second {
		t.Fatalf("took %s", d)
	}
}

func TestRetryDoesNotCallAgainWhenTheContextEndsDuringTheWait(t *testing.T) {
	u, _, _, _ := setup()
	u.Retries = 5
	u.Backoff = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	err := u.retry(ctx, "x", func(context.Context) error {
		calls++
		time.AfterFunc(10*time.Millisecond, cancel)
		return errors.New("injected failure")
	})
	if err == nil || calls != 1 {
		t.Fatalf("got %v after %d calls", err, calls)
	}
}

// flakyHeadS3 fails the first HeadObject call.
type flakyHeadS3 struct {
	*fake.S3
	failed atomic.Bool
}

func (s *flakyHeadS3) HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	if s.failed.CompareAndSwap(false, true) {
		return nil, errors.New("injected failure")
	}
	return s.S3.HeadObject(ctx, in, opts...)
}

func TestUploadTriesAFailedHeadObjectAgain(t *testing.T) {
	u, s3, src, f := setup()
	u.API = &flakyHeadS3{S3: s3}
	if _, err := u.Upload(context.Background(), src, f, "k"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s3.Objects["k"], data) {
		t.Fatalf("object is %q", s3.Objects["k"])
	}
}

// lostCompleteS3 completes the upload but returns an error, as when the
// response is lost.
type lostCompleteS3 struct{ *fake.S3 }

func (s *lostCompleteS3) CompleteMultipartUpload(ctx context.Context, in *s3.CompleteMultipartUploadInput, opts ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	s.S3.CompleteMultipartUpload(ctx, in, opts...)
	return nil, errors.New("injected lost response")
}

func TestUploadCountsALostCompleteResponseAsUploadedWhenTheObjectExists(t *testing.T) {
	u, s3, src, f := setup()
	u.API = &lostCompleteS3{S3: s3}
	res, err := u.Upload(context.Background(), src, f, "k")
	if err != nil || res != Uploaded {
		t.Fatalf("got %v, %v", res, err)
	}
	if !bytes.Equal(s3.Objects["k"], data) {
		t.Fatalf("object is %q", s3.Objects["k"])
	}
}

// failedFirstRun stores parts 1 and 2 of data and leaves the upload open.
func failedFirstRun(t *testing.T, u *Uploader, s3 *fake.S3, src source.Source, f source.File) {
	t.Helper()
	u.Workers = 1
	s3.FailPart = failPart(3)
	if _, err := u.Upload(context.Background(), src, f, "k"); err == nil {
		t.Fatal("the first run must fail")
	}
	s3.FailPart = nil
	s3.PartCalls = 0
}

func TestUploadResumeStartsAgainWhenTheFirstStoredPartChanged(t *testing.T) {
	u, s3, src, f := setup()
	failedFirstRun(t, u, s3, src, f)
	changed := bytes.Clone(data)
	changed[1] = 'X' // part 1; same size and same time
	src.Files["a.mov"] = changed
	if _, err := u.Upload(context.Background(), src, f, "k"); err != nil {
		t.Fatal(err)
	}
	if s3.PartCalls != 4 || s3.Open() != 0 || !bytes.Equal(s3.Objects["k"], changed) {
		t.Fatalf("%d part calls, want 4; %d open uploads; object %q", s3.PartCalls, s3.Open(), s3.Objects["k"])
	}
}

func TestUploadResumeStartsAgainWhenTheHighestStoredPartChanged(t *testing.T) {
	u, s3, src, f := setup()
	failedFirstRun(t, u, s3, src, f)
	changed := bytes.Clone(data)
	changed[6] = 'X' // part 2; same size and same time
	src.Files["a.mov"] = changed
	if _, err := u.Upload(context.Background(), src, f, "k"); err != nil {
		t.Fatal(err)
	}
	if s3.PartCalls != 4 || s3.Open() != 0 || !bytes.Equal(s3.Objects["k"], changed) {
		t.Fatalf("%d part calls, want 4; %d open uploads; object %q", s3.PartCalls, s3.Open(), s3.Objects["k"])
	}
}

func TestUploadSetsTheContentTypeFromTheFileExtension(t *testing.T) {
	cases := []struct{ key, want string }{
		{"deck/a.mp4", "video/mp4"}, // multipart
		{"deck/a.MOV", "video/quicktime"},
		{"deck/a.mxf", "application/mxf"},
		{"deck/a.wav", "audio/wav"},
		{"deck/a.braw", ""},
		{"deck/noext", ""},
	}
	for _, c := range cases {
		for _, size := range []int{len(data), 3} { // multipart and small
			u, s3, _, _ := setup()
			body := data[:size]
			src := &fake.Source{Files: map[string][]byte{"f": body}, ModTime: time.Now().Add(-time.Hour)}
			f := source.File{Path: "f", Size: int64(size), ModTime: src.ModTime}
			if _, err := u.Upload(context.Background(), src, f, c.key); err != nil {
				t.Fatal(err)
			}
			if got := s3.ContentTypes[c.key]; got != c.want {
				t.Errorf("%s with %d bytes: content type %q, want %q", c.key, size, got, c.want)
			}
		}
	}
}

var errNoSuchUpload = &smithy.GenericAPIError{Code: "NoSuchUpload", Message: "the upload does not exist"}

// lostAbortS3 aborts the first call but returns an error, as when the
// response is lost. Later calls get NoSuchUpload.
type lostAbortS3 struct {
	*fake.S3
	calls atomic.Int32
}

func (s *lostAbortS3) AbortMultipartUpload(ctx context.Context, in *s3.AbortMultipartUploadInput, opts ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	if s.calls.Add(1) == 1 {
		s.S3.AbortMultipartUpload(ctx, in, opts...)
		return nil, errors.New("injected lost response")
	}
	return nil, errNoSuchUpload
}

func TestAbortCountsNoSuchUploadAfterALostResponseAsDone(t *testing.T) {
	u, store, _, _ := setup()
	w := &lostAbortS3{S3: store}
	u.API = w
	out, err := store.CreateMultipartUpload(context.Background(), &s3.CreateMultipartUploadInput{Bucket: aws.String("b"), Key: aws.String("k")})
	if err != nil {
		t.Fatal(err)
	}
	if err := u.abort(context.Background(), "k", out.UploadId); err != nil {
		t.Fatal(err)
	}
	if store.Open() != 0 || w.calls.Load() != 2 {
		t.Fatalf("%d open uploads, %d abort calls", store.Open(), w.calls.Load())
	}
}

// lostFirstCompleteS3 stores the object on the first Complete but returns an
// error. Later calls get NoSuchUpload.
type lostFirstCompleteS3 struct {
	*fake.S3
	calls atomic.Int32
}

func (s *lostFirstCompleteS3) CompleteMultipartUpload(ctx context.Context, in *s3.CompleteMultipartUploadInput, opts ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	if s.calls.Add(1) == 1 {
		s.S3.CompleteMultipartUpload(ctx, in, opts...)
		return nil, errors.New("injected lost response")
	}
	return nil, errNoSuchUpload
}

func TestUploadStopsCompleteTriesOnNoSuchUploadAndChecksTheObjectSize(t *testing.T) {
	u, s3, src, f := setup()
	u.Retries = 5
	w := &lostFirstCompleteS3{S3: s3}
	u.API = w
	res, err := u.Upload(context.Background(), src, f, "k")
	if err != nil || res != Uploaded {
		t.Fatalf("got %v, %v", res, err)
	}
	if n := w.calls.Load(); n > 2 {
		t.Fatalf("%d complete calls", n)
	}
}

// flakySizeSource fails the first Size call.
type flakySizeSource struct {
	*fake.Source
	failed atomic.Bool
}

func (s *flakySizeSource) Size(ctx context.Context, path string) (int64, error) {
	if s.failed.CompareAndSwap(false, true) {
		return 0, errors.New("injected failure")
	}
	return s.Source.Size(ctx, path)
}

func TestUploadTriesAFailedSizeCallAgain(t *testing.T) {
	u, s3, src, f := setup()
	if _, err := u.Upload(context.Background(), &flakySizeSource{Source: src}, f, "k"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s3.Objects["k"], data) {
		t.Fatalf("object is %q", s3.Objects["k"])
	}
}

// lateCancelContext reports the end only after the first Err call, so the
// wait in retry can see Done and the timer ready together.
type lateCancelContext struct {
	context.Context
	errCalls atomic.Int32
}

func (c *lateCancelContext) Err() error {
	if c.errCalls.Add(1) == 1 {
		return nil
	}
	return context.Canceled
}

func (c *lateCancelContext) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func TestRetryDoesNotCallAgainWhenTheContextAndTheTimerAreReadyTogether(t *testing.T) {
	u, _, _, _ := setup()
	u.Retries = 5
	u.Backoff = time.Nanosecond
	for range 200 {
		calls := 0
		ctx := &lateCancelContext{Context: context.Background()}
		err := u.retry(ctx, "x", func(context.Context) error {
			calls++
			return errors.New("injected failure")
		})
		if err == nil || calls != 1 {
			t.Fatalf("got %v after %d calls", err, calls)
		}
	}
}
