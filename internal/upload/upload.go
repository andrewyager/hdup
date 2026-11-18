// Package upload sends one file from a source to S3 and continues a
// multipart upload that was interrupted.
package upload

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/andrewyager/hdup/internal/plan"
	"github.com/andrewyager/hdup/internal/source"
)

// API is the set of S3 calls that the uploader uses. *s3.Client satisfies it.
type API interface {
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	CreateMultipartUpload(context.Context, *s3.CreateMultipartUploadInput, ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	UploadPart(context.Context, *s3.UploadPartInput, ...func(*s3.Options)) (*s3.UploadPartOutput, error)
	CompleteMultipartUpload(context.Context, *s3.CompleteMultipartUploadInput, ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
	ListMultipartUploads(context.Context, *s3.ListMultipartUploadsInput, ...func(*s3.Options)) (*s3.ListMultipartUploadsOutput, error)
	ListParts(context.Context, *s3.ListPartsInput, ...func(*s3.Options)) (*s3.ListPartsOutput, error)
}

type Uploader struct {
	API         API
	Bucket      string
	PartSize    int64
	Workers     int           // parallel parts for one file
	Retries     int           // tries for each read and each S3 call
	Backoff     time.Duration // first delay between tries; doubles, 30 s maximum
	IdleTimeout time.Duration // a send try stops when it moves no data for this time
	Limit       *Limiter      // optional: the total send rate of all Uploaders that share it

	// Optional progress hooks. They can be called from more than one
	// goroutine. Read and Sent can get a negative value: it gives back the
	// bytes of a try that failed.
	Read    func(n int64) // bytes read from the source
	Sent    func(n int64) // bytes sent to S3
	Resumed func(n int64) // bytes that an earlier run sent
}

// controlTimeout is the time limit for one try of an S3 call that sends no
// file data.
var controlTimeout = 2 * time.Minute

type Result int

const (
	Uploaded Result = iota
	Skipped
)

func call(hook func(int64), n int64) {
	if hook != nil {
		hook(n)
	}
}

func notFound(err error) bool {
	var nf *types.NotFound
	var status interface{ HTTPStatusCode() int }
	return errors.As(err, &nf) || (errors.As(err, &status) && status.HTTPStatusCode() == 404)
}

var contentTypes = map[string]string{
	".mp4": "video/mp4",
	".mov": "video/quicktime",
	".mxf": "application/mxf",
	".wav": "audio/wav",
}

// contentType gives the content type for the extension of key, or nil if it
// is not known.
func contentType(key string) *string {
	ext := strings.ToLower(path.Ext(key))
	t, ok := contentTypes[ext]
	if !ok {
		t = mime.TypeByExtension(ext)
	}
	if t == "" {
		return nil
	}
	return &t
}

func noSuchUpload(err error) bool {
	var api smithy.APIError
	return errors.As(err, &api) && api.ErrorCode() == "NoSuchUpload"
}

// noRetry marks an error that more tries cannot fix.
type noRetry struct{ error }

func (e noRetry) Unwrap() error { return e.error }

func contentMD5(b []byte) *string {
	sum := md5.Sum(b)
	return aws.String(base64.StdEncoding.EncodeToString(sum[:]))
}

// retry calls fn until it succeeds, ctx ends, or the tries are used.
func (u *Uploader) retry(ctx context.Context, what string, fn func(ctx context.Context) error) error {
	delay := u.Backoff
	for try := 1; ; try++ {
		err := fn(ctx)
		if err == nil {
			return nil
		}
		var final noRetry
		if ctx.Err() != nil || try >= u.Retries || errors.As(err, &final) {
			return fmt.Errorf("%s: %w", what, err)
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		delay = min(delay*2, 30*time.Second)
	}
}

// control calls retry for an S3 call that sends no file data. Each try has
// a time limit.
func (u *Uploader) control(ctx context.Context, what string, fn func(ctx context.Context) error) error {
	return u.retry(ctx, what, func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, controlTimeout)
		defer cancel()
		return fn(ctx)
	})
}

var errIdle = errors.New("no data moved")

// send makes one send try. fn must send body. The try stops when body gets
// no Read for IdleTimeout and fn has not returned.
func (u *Uploader) send(ctx context.Context, buf []byte, fn func(ctx context.Context, body io.ReadSeeker) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	b := newBody(buf, u.Sent, u.IdleTimeout, func() { cancel(errIdle) }, u.Limit)
	err := fn(ctx, b)
	b.finish(err == nil)
	if err != nil && errors.Is(context.Cause(ctx), errIdle) {
		return fmt.Errorf("no data sent for %s: %w", u.IdleTimeout, err)
	}
	return err
}

// read fills buf from offset of f and tells the Read hook. If the source is a
// ProgressReader, the hook gets the bytes while they move, and a failed try
// gives back its bytes.
func (u *Uploader) read(ctx context.Context, src source.Source, f source.File, what string, offset int64, buf []byte) error {
	pr, ok := src.(source.ProgressReader)
	if !ok {
		err := u.retry(ctx, what, func(ctx context.Context) error {
			return src.ReadRange(ctx, f.Path, offset, buf)
		})
		if err == nil {
			call(u.Read, int64(len(buf)))
		}
		return err
	}
	return u.retry(ctx, what, func(ctx context.Context) error {
		var n int64
		err := pr.ReadRangeProgress(ctx, f.Path, offset, buf, func(c int64) {
			n += c
			call(u.Read, c)
		})
		if err != nil && n != 0 {
			call(u.Read, -n)
		}
		return err
	})
}

// checkSize fails if the size of f on the source is not the listed size.
func (u *Uploader) checkSize(ctx context.Context, src source.Source, f source.File, key string) error {
	var size int64
	err := u.retry(ctx, key+": size", func(ctx context.Context) error {
		var err error
		size, err = src.Size(ctx, f.Path)
		return err
	})
	if err != nil {
		return err
	}
	if size != f.Size {
		return fmt.Errorf("%s: the source size is %d, the list gave %d; the file is possibly recording", key, size, f.Size)
	}
	return nil
}

// objectSize returns the size of the object at key, or -1 if there is no object.
func (u *Uploader) objectSize(ctx context.Context, key string) (int64, error) {
	size := int64(-1)
	err := u.control(ctx, key+": head", func(ctx context.Context) error {
		head, err := u.API.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &u.Bucket, Key: &key})
		switch {
		case notFound(err):
			size = -1
			return nil
		case err != nil:
			return err
		}
		size = aws.ToInt64(head.ContentLength)
		return nil
	})
	return size, err
}

// Upload sends f to key. A second call continues where the first one stopped.
func (u *Uploader) Upload(ctx context.Context, src source.Source, f source.File, key string) (Result, error) {
	if err := u.checkSize(ctx, src, f, key); err != nil {
		return 0, err
	}
	size, err := u.objectSize(ctx, key)
	if err != nil {
		return 0, err
	}
	if size == f.Size {
		return Skipped, nil
	}
	parts := plan.Parts(f.Size, u.PartSize)
	if len(parts) <= 1 {
		return Uploaded, u.putSmall(ctx, src, f, key)
	}

	id, done, err := u.resume(ctx, src, f, key, parts)
	if err != nil {
		return 0, fmt.Errorf("%s: resume: %w", key, err)
	}
	if id == "" {
		err := u.control(ctx, key+": create multipart upload", func(ctx context.Context) error {
			out, err := u.API.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &u.Bucket, Key: &key, ContentType: contentType(key)})
			if err != nil {
				return err
			}
			id = aws.ToString(out.UploadId)
			return nil
		})
		if err != nil {
			return 0, err
		}
	}

	etags, err := u.sendParts(ctx, src, f, key, id, parts, done)
	if err != nil {
		return 0, err
	}
	if err := u.checkSize(ctx, src, f, key); err != nil {
		return 0, err
	}

	completed := make([]types.CompletedPart, len(parts))
	for i, p := range parts {
		completed[i] = types.CompletedPart{PartNumber: aws.Int32(p.Number), ETag: aws.String(etags[p.Number])}
	}
	err = u.control(ctx, key+": complete", func(ctx context.Context) error {
		_, err := u.API.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
			Bucket: &u.Bucket, Key: &key, UploadId: &id,
			MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
		})
		if noSuchUpload(err) {
			// An earlier try can have worked and lost its response.
			return noRetry{err}
		}
		return err
	})
	if err != nil {
		// A Complete that worked can lose its response.
		if size, herr := u.objectSize(ctx, key); herr == nil && size == f.Size {
			return Uploaded, nil
		}
		return 0, err
	}
	return Uploaded, nil
}

func (u *Uploader) putSmall(ctx context.Context, src source.Source, f source.File, key string) error {
	buf := make([]byte, f.Size)
	if err := u.read(ctx, src, f, key+": read", 0, buf); err != nil {
		return err
	}
	if err := u.checkSize(ctx, src, f, key); err != nil {
		return err
	}
	sum := contentMD5(buf)
	return u.retry(ctx, key+": put", func(ctx context.Context) error {
		return u.send(ctx, buf, func(ctx context.Context, body io.ReadSeeker) error {
			_, err := u.API.PutObject(ctx, &s3.PutObjectInput{
				Bucket: &u.Bucket, Key: &key, Body: body,
				ContentLength: aws.Int64(f.Size), ContentMD5: sum, ContentType: contentType(key),
			})
			return err
		})
	})
}

func (u *Uploader) abort(ctx context.Context, key string, id *string) error {
	return u.control(ctx, key+": abort", func(ctx context.Context) error {
		_, err := u.API.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &u.Bucket, Key: &key, UploadId: id})
		if noSuchUpload(err) {
			// An earlier try worked and lost its response.
			return nil
		}
		return err
	})
}

// resume finds a multipart upload that can continue. It returns the upload
// ID and the ETag of each part that is stored. An empty ID means that a new
// upload is necessary.
func (u *Uploader) resume(ctx context.Context, src source.Source, f source.File, key string, parts []plan.Part) (string, map[int32]string, error) {
	var found []types.MultipartUpload
	uploads := s3.NewListMultipartUploadsPaginator(u.API, &s3.ListMultipartUploadsInput{Bucket: &u.Bucket, Prefix: &key})
	for uploads.HasMorePages() {
		var page *s3.ListMultipartUploadsOutput
		err := u.control(ctx, "list multipart uploads", func(ctx context.Context) error {
			var err error
			page, err = uploads.NextPage(ctx)
			return err
		})
		if err != nil {
			return "", nil, err
		}
		for _, up := range page.Uploads {
			if aws.ToString(up.Key) == key {
				found = append(found, up)
			}
		}
	}
	sort.Slice(found, func(i, j int) bool {
		return aws.ToTime(found[i].Initiated).After(aws.ToTime(found[j].Initiated))
	})
	id := ""
	for _, up := range found {
		// An upload that started before the source changed has old data.
		if id == "" && aws.ToTime(up.Initiated).After(f.ModTime) {
			id = aws.ToString(up.UploadId)
			continue
		}
		if err := u.abort(ctx, key, up.UploadId); err != nil {
			return "", nil, err
		}
	}
	if id == "" {
		return "", nil, nil
	}

	want := make(map[int32]int64, len(parts))
	for _, p := range parts {
		want[p.Number] = p.Size
	}
	done := map[int32]string{}
	stored := s3.NewListPartsPaginator(u.API, &s3.ListPartsInput{Bucket: &u.Bucket, Key: &key, UploadId: &id})
	for stored.HasMorePages() {
		var page *s3.ListPartsOutput
		err := u.control(ctx, "list parts", func(ctx context.Context) error {
			var err error
			page, err = stored.NextPage(ctx)
			return err
		})
		if err != nil {
			return "", nil, err
		}
		for _, p := range page.Parts {
			n := aws.ToInt32(p.PartNumber)
			if size, ok := want[n]; !ok || size != aws.ToInt64(p.Size) {
				// The earlier run used a different part size.
				return "", nil, u.abort(ctx, key, &id)
			}
			done[n] = aws.ToString(p.ETag)
		}
	}
	same, err := u.verify(ctx, src, f, key, parts, done)
	if err != nil {
		return "", nil, err
	}
	if !same {
		return "", nil, u.abort(ctx, key, &id)
	}
	return id, done, nil
}

// verify reads the lowest and the highest stored part from the source and
// compares their MD5 with the stored ETag. The modified time on a deck has
// minute precision and comes from a different clock than S3, so a file that
// changed with the same size can have old parts.
func (u *Uploader) verify(ctx context.Context, src source.Source, f source.File, key string, parts []plan.Part, done map[int32]string) (bool, error) {
	if len(done) == 0 {
		return true, nil
	}
	lo, hi := parts[len(parts)-1].Number, parts[0].Number
	for n := range done {
		lo, hi = min(lo, n), max(hi, n)
	}
	check := []int32{lo}
	if hi != lo {
		check = append(check, hi)
	}
	buf := make([]byte, parts[0].Size)
	for _, n := range check {
		p := parts[n-1]
		b := buf[:p.Size]
		err := u.retry(ctx, fmt.Sprintf("%s part %d: verify", key, n), func(ctx context.Context) error {
			return src.ReadRange(ctx, f.Path, p.Offset, b)
		})
		if err != nil {
			return false, err
		}
		sum := md5.Sum(b)
		if hex.EncodeToString(sum[:]) != strings.ToLower(strings.Trim(done[n], `"`)) {
			return false, nil
		}
	}
	return true, nil
}

// sendParts sends each part that is not in done. It returns the ETag of all parts.
func (u *Uploader) sendParts(ctx context.Context, src source.Source, f source.File, key, id string, parts []plan.Part, done map[int32]string) (map[int32]string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	etags := make(map[int32]string, len(parts))
	for n, e := range done {
		etags[n] = e
	}
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	todo := make(chan plan.Part)
	for range min(u.Workers, len(parts)) {
		wg.Go(func() {
			buf := make([]byte, parts[0].Size)
			for p := range todo {
				if ctx.Err() != nil {
					continue // a different part failed; send no more parts
				}
				etag, err := u.sendPart(ctx, src, f, key, id, p, buf[:p.Size])
				mu.Lock()
				if err == nil {
					etags[p.Number] = etag
				} else if firstErr == nil {
					firstErr = err
					cancel()
				}
				mu.Unlock()
			}
		})
	}
feed:
	for _, p := range parts {
		if _, ok := done[p.Number]; ok {
			call(u.Resumed, p.Size)
			continue
		}
		select {
		case todo <- p:
		case <-ctx.Done():
			break feed
		}
	}
	close(todo)
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	return etags, nil
}

func (u *Uploader) sendPart(ctx context.Context, src source.Source, f source.File, key, id string, p plan.Part, buf []byte) (string, error) {
	what := fmt.Sprintf("%s part %d", key, p.Number)
	if err := u.read(ctx, src, f, what+": read", p.Offset, buf); err != nil {
		return "", err
	}
	sum := contentMD5(buf)
	var etag string
	err := u.retry(ctx, what+": send", func(ctx context.Context) error {
		return u.send(ctx, buf, func(ctx context.Context, body io.ReadSeeker) error {
			out, err := u.API.UploadPart(ctx, &s3.UploadPartInput{
				Bucket: &u.Bucket, Key: &key, UploadId: &id, PartNumber: aws.Int32(p.Number),
				Body: body, ContentLength: aws.Int64(p.Size), ContentMD5: sum,
			})
			if err != nil {
				return err
			}
			etag = aws.ToString(out.ETag)
			return nil
		})
	})
	if err != nil {
		return "", err
	}
	return etag, nil
}
