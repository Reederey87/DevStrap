package hub

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"
)

// TestR2MinIOConformance runs the shared hub conformance contract
// (assertHubRoundTrip) against a live MinIO/R2 bucket via the production
// aws-sdk-go-v2 S3Adapter (P5-HUB-01). It is env-gated — not a build tag — so the
// file always compiles (a refactor cannot silently break it and `go mod tidy`
// keeps the SDK a stable direct require) while CI stays hermetic.
//
// Set DEVSTRAP_HUB_S3_ENDPOINT plus creds to run it, e.g. against a 2024+ MinIO
// image that supports conditional puts (If-None-Match: *):
//
//	docker run -p 9000:9000 minio/minio server /data
//	DEVSTRAP_HUB_S3_ENDPOINT=http://localhost:9000 \
//	DEVSTRAP_HUB_S3_ACCESS_KEY_ID=minioadmin \
//	DEVSTRAP_HUB_S3_SECRET_ACCESS_KEY=minioadmin \
//	go test -run TestR2MinIOConformance ./internal/hub
func TestR2MinIOConformance(t *testing.T) {
	endpoint := os.Getenv("DEVSTRAP_HUB_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set DEVSTRAP_HUB_S3_ENDPOINT to run the live MinIO/R2 integration test (P5-HUB-01)")
	}
	accessKey := os.Getenv("DEVSTRAP_HUB_S3_ACCESS_KEY_ID")
	secretKey := os.Getenv("DEVSTRAP_HUB_S3_SECRET_ACCESS_KEY")
	if accessKey == "" || secretKey == "" {
		t.Skip("DEVSTRAP_HUB_S3_ACCESS_KEY_ID / DEVSTRAP_HUB_S3_SECRET_ACCESS_KEY not set")
	}
	bucket := os.Getenv("DEVSTRAP_HUB_S3_BUCKET")
	if bucket == "" {
		bucket = "devstrap-test"
	}
	region := os.Getenv("DEVSTRAP_HUB_S3_REGION")
	if region == "" {
		region = "auto"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Create the bucket first (swallow already-exists) via a management client.
	creds := aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secretKey}
	mgmt := s3.New(s3.Options{
		Region:       region,
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		Retryer:      aws.NopRetryer{},
		Credentials:  aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) { return creds, nil }),
	})
	if _, err := mgmt.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		var exists *types.BucketAlreadyExists
		var owned *types.BucketAlreadyOwnedByYou
		if !errors.As(err, &exists) && !errors.As(err, &owned) {
			t.Fatalf("CreateBucket %s: %v", bucket, err)
		}
	}

	// A random workspace isolates this run's objects under their own prefix so
	// repeated runs against a shared test bucket do not interfere.
	adapter, err := NewS3Client(endpoint, region, bucket, accessKey, secretKey)
	if err != nil {
		t.Fatalf("NewS3Client: %v", err)
	}
	h := R2Hub{S3: adapter, WorkspaceID: "ws_minio_" + uuid.NewString()}

	// The production adapter must satisfy the SAME conformance contract as the
	// in-memory double (TestR2ConformanceMemS3).
	assertHubRoundTrip(t, h)
}
