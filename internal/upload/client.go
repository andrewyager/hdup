package upload

import (
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/andrewyager/hdup/internal/job"
)

// NewClient makes an S3 client for Wasabi that sends all requests through hc.
func NewClient(r job.S3Remote, hc *http.Client) *s3.Client {
	return s3.New(s3.Options{
		Region:       r.Region,
		BaseEndpoint: aws.String("https://" + r.Endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider(r.AccessKey, r.SecretKey, ""),
		HTTPClient:   hc,
		// Path style keeps all requests on the one endpoint host name.
		UsePathStyle: true,
		// The uploader does its own tries.
		RetryMaxAttempts: 1,
		// The uploader sends Content-MD5. The default CRC trailers of the SDK
		// are for AWS and are not necessary here.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
}
