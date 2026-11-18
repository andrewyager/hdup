package probe

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/andrewyager/hdup/internal/fake"
	"github.com/andrewyager/hdup/internal/upload"
)

func TestRunMeasuresEachCaseAndLeavesNothingInTheBucket(t *testing.T) {
	s3 := fake.NewS3()
	var got [][2]int // addresses and connections for each case
	var out bytes.Buffer
	err := Run(context.Background(), Config{
		Bucket:   "b",
		Addrs:    []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"},
		Duration: 50 * time.Millisecond,
		PartSize: 1024,
		NewAPI: func(addrs []string, conns int) upload.API {
			got = append(got, [2]int{len(addrs), conns})
			return s3
		},
		Out: &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Cases ask for 8 addresses; only 3 are available.
	want := [][2]int{{1, 1}, {1, 8}, {3, 8}, {3, 32}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("case %d: got %v, want %v", i, got[i], want[i])
		}
	}
	if s3.Open() != 0 || len(s3.Objects) != 0 {
		t.Fatalf("%d open uploads, %d objects", s3.Open(), len(s3.Objects))
	}
	if s3.PartCalls == 0 || strings.Count(out.String(), "Mbit/s") != 4 {
		t.Fatalf("%d part calls, output:\n%s", s3.PartCalls, out.String())
	}
}

type failingAbort struct{ *fake.S3 }

func (failingAbort) AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	return nil, errors.New("network down")
}

func TestRunReturnsAnErrorWithTheKeyWhenTheAbortFails(t *testing.T) {
	api := failingAbort{fake.NewS3()}
	err := Run(context.Background(), Config{
		Bucket:   "b",
		Addrs:    []string{"10.0.0.1"},
		Duration: 20 * time.Millisecond,
		PartSize: 1024,
		NewAPI:   func([]string, int) upload.API { return api },
		Out:      &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "hdup-probe/") {
		t.Fatalf("got %v", err)
	}
}
