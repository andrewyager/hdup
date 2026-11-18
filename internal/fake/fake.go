// Package fake has S3 and source substitutes in memory, for tests.
package fake

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/andrewyager/hdup/internal/source"
)

type S3 struct {
	mu      sync.Mutex
	Objects map[string][]byte
	// ContentTypes holds the content type that each stored object got.
	ContentTypes map[string]string
	Now          func() time.Time
	FailPart     func(part int32) error // optional, called for each UploadPart
	PartCalls    int
	uploads      map[string]*multipart
	seq          int
}

type multipart struct {
	key         string
	contentType string
	initiated   time.Time
	parts       map[int32][]byte
}

func NewS3() *S3 {
	return &S3{Objects: map[string][]byte{}, ContentTypes: map[string]string{}, Now: time.Now, uploads: map[string]*multipart{}}
}

// Open returns the number of multipart uploads that are not complete.
func (s *S3) Open() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.uploads)
}

func etag(b []byte) string {
	sum := md5.Sum(b)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func checkMD5(body []byte, contentMD5 *string) error {
	sum := md5.Sum(body)
	if aws.ToString(contentMD5) != base64.StdEncoding.EncodeToString(sum[:]) {
		return errors.New("BadDigest")
	}
	return nil
}

func (s *S3) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.Objects[*in.Key]
	if !ok {
		return nil, &types.NotFound{}
	}
	return &s3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(b)))}, nil
}

func (s *S3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	if err := checkMD5(body, in.ContentMD5); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Objects[*in.Key] = body
	s.ContentTypes[*in.Key] = aws.ToString(in.ContentType)
	return &s3.PutObjectOutput{ETag: aws.String(etag(body))}, nil
}

func (s *S3) CreateMultipartUpload(_ context.Context, in *s3.CreateMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	id := fmt.Sprintf("upload-%d", s.seq)
	s.uploads[id] = &multipart{key: *in.Key, contentType: aws.ToString(in.ContentType), initiated: s.Now(), parts: map[int32][]byte{}}
	return &s3.CreateMultipartUploadOutput{UploadId: aws.String(id)}, nil
}

func (s *S3) UploadPart(_ context.Context, in *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.PartCalls++
	if s.FailPart != nil {
		if err := s.FailPart(*in.PartNumber); err != nil {
			return nil, err
		}
	}
	up, ok := s.uploads[*in.UploadId]
	if !ok {
		return nil, errors.New("NoSuchUpload")
	}
	if err := checkMD5(body, in.ContentMD5); err != nil {
		return nil, err
	}
	up.parts[*in.PartNumber] = body
	return &s3.UploadPartOutput{ETag: aws.String(etag(body))}, nil
}

func (s *S3) CompleteMultipartUpload(_ context.Context, in *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	up, ok := s.uploads[*in.UploadId]
	if !ok {
		return nil, errors.New("NoSuchUpload")
	}
	var object []byte
	for _, p := range in.MultipartUpload.Parts {
		body, ok := up.parts[*p.PartNumber]
		if !ok || etag(body) != aws.ToString(p.ETag) {
			return nil, errors.New("InvalidPart")
		}
		object = append(object, body...)
	}
	s.Objects[up.key] = object
	s.ContentTypes[up.key] = up.contentType
	delete(s.uploads, *in.UploadId)
	return &s3.CompleteMultipartUploadOutput{}, nil
}

func (s *S3) AbortMultipartUpload(_ context.Context, in *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.uploads, *in.UploadId)
	return &s3.AbortMultipartUploadOutput{}, nil
}

func (s *S3) ListMultipartUploads(_ context.Context, in *s3.ListMultipartUploadsInput, _ ...func(*s3.Options)) (*s3.ListMultipartUploadsOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &s3.ListMultipartUploadsOutput{}
	for id, up := range s.uploads {
		if strings.HasPrefix(up.key, aws.ToString(in.Prefix)) {
			out.Uploads = append(out.Uploads, types.MultipartUpload{
				Key: aws.String(up.key), UploadId: aws.String(id), Initiated: aws.Time(up.initiated),
			})
		}
	}
	return out, nil
}

func (s *S3) ListParts(_ context.Context, in *s3.ListPartsInput, _ ...func(*s3.Options)) (*s3.ListPartsOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	up, ok := s.uploads[*in.UploadId]
	if !ok {
		return nil, errors.New("NoSuchUpload")
	}
	out := &s3.ListPartsOutput{}
	for n, body := range up.parts {
		out.Parts = append(out.Parts, types.Part{
			PartNumber: aws.Int32(n), Size: aws.Int64(int64(len(body))), ETag: aws.String(etag(body)),
		})
	}
	sort.Slice(out.Parts, func(i, j int) bool { return *out.Parts[i].PartNumber < *out.Parts[j].PartNumber })
	return out, nil
}

// Source is a source.Source in memory.
type Source struct {
	Files     map[string][]byte
	ModTime   time.Time
	SizeDelta int64 // added to the result of Size, to simulate a changed file
}

func (s *Source) List(context.Context) ([]source.File, error) {
	var out []source.File
	for p, b := range s.Files {
		out = append(out, source.File{Path: p, Size: int64(len(b)), ModTime: s.ModTime})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (s *Source) Size(_ context.Context, path string) (int64, error) {
	return int64(len(s.Files[path])) + s.SizeDelta, nil
}

func (s *Source) ReadRange(_ context.Context, path string, offset int64, buf []byte) error {
	b := s.Files[path]
	if offset+int64(len(buf)) > int64(len(b)) {
		return io.ErrUnexpectedEOF
	}
	copy(buf, b[offset:])
	return nil
}
