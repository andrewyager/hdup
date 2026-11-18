// Package probe measures the upload rate for different numbers of
// connections and addresses.
package probe

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/andrewyager/hdup/internal/plan"
	"github.com/andrewyager/hdup/internal/upload"
)

type Case struct{ Conns, Addrs int }

var Cases = []Case{{1, 1}, {8, 1}, {8, 8}, {32, 8}}

type Config struct {
	Bucket   string
	Addrs    []string
	Duration time.Duration // time for each case
	PartSize int64
	// NewAPI makes a client that uses only addrs and keeps conns connections.
	NewAPI func(addrs []string, conns int) upload.API
	Out    io.Writer
}

func Run(ctx context.Context, cfg Config) error {
	data := make([]byte, cfg.PartSize)
	rand.Read(data)
	fmt.Fprintf(cfg.Out, "%11s  %9s  %s\n", "connections", "addresses", "rate")
	for _, c := range Cases {
		n := min(c.Addrs, len(cfg.Addrs))
		rate, err := measure(ctx, cfg, cfg.NewAPI(cfg.Addrs[:n], c.Conns), c.Conns, data)
		if err != nil {
			return err
		}
		fmt.Fprintf(cfg.Out, "%11d  %9d  %.0f Mbit/s\n", c.Conns, n, rate)
	}
	return nil
}

// measure returns the rate in Mbit/s.
func measure(ctx context.Context, cfg Config, api upload.API, conns int, data []byte) (rate float64, err error) {
	key := fmt.Sprintf("hdup-probe/%d", time.Now().UnixNano())
	created, err := api.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &cfg.Bucket, Key: &key})
	if err != nil {
		return 0, fmt.Errorf("probe: create multipart upload: %w", err)
	}
	defer func() {
		// Abort also after Ctrl-C, so that no parts stay in the bucket.
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, aerr := api.AbortMultipartUpload(actx, &s3.AbortMultipartUploadInput{Bucket: &cfg.Bucket, Key: &key, UploadId: created.UploadId})
		if aerr != nil {
			aerr = fmt.Errorf("probe: abort of upload %s for %s failed; remove it with \"rclone cleanup\": %w", aws.ToString(created.UploadId), key, aerr)
			rate, err = 0, errors.Join(err, aerr)
		}
	}()

	sum := md5.Sum(data)
	contentMD5 := aws.String(base64.StdEncoding.EncodeToString(sum[:]))
	runCtx, cancel := context.WithTimeout(ctx, cfg.Duration)
	defer cancel()

	var (
		sent     atomic.Int64
		next     atomic.Int32
		errOnce  sync.Once
		firstErr error
		wg       sync.WaitGroup
	)
	start := time.Now()
	for range conns {
		wg.Go(func() {
			for runCtx.Err() == nil {
				n := next.Add(1)
				if n > plan.MaxParts {
					return
				}
				_, err := api.UploadPart(runCtx, &s3.UploadPartInput{
					Bucket: &cfg.Bucket, Key: &key, UploadId: created.UploadId, PartNumber: aws.Int32(n),
					Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data))), ContentMD5: contentMD5,
				})
				if err != nil {
					if runCtx.Err() == nil {
						errOnce.Do(func() { firstErr = fmt.Errorf("probe: upload part: %w", err); cancel() })
					}
					return
				}
				sent.Add(int64(len(data)))
			}
		})
	}
	wg.Wait()
	if firstErr != nil {
		return 0, firstErr
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return float64(sent.Load()) * 8 / 1e6 / time.Since(start).Seconds(), nil
}
