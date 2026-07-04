// s3client_awssdk.go is the production S3Client adapter (P5-HUB-01). It wraps
// the aws-sdk-go-v2 S3 client pointed at a Cloudflare R2 (or any S3-compatible)
// endpoint and translates SDK errors into the four hub sentinels
// (ErrPreconditionFailed, ErrS3Throttle, ErrS3Transient, dssync.ErrBlobNotFound)
// so R2Hub.Retry is the single retry/classification layer.
//
// It is constructed with s3.New(s3.Options{...}) — not config.LoadDefaultConfig
// — to keep the dependency/govulncheck surface to the S3 data path only (no
// SSO/IMDS/STS chain). The SDK's own retryer is disabled (aws.NopRetryer{}) so
// R2Hub.Retry is the ONLY retry layer and there is no double-retry/billing loop.
// Credentials are supplied inline via aws.CredentialsProviderFunc so the
// `credentials` module is never pulled in.
package hub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

// S3Adapter is the production hub.S3Client backed by aws-sdk-go-v2 (P5-HUB-01).
// It points at one bucket; workspace key-prefix scoping is handled by R2Hub's
// keying (workspaces/<workspace_id>/...), so the adapter itself is prefix-free.
type S3Adapter struct {
	client *s3.Client
	bucket string
}

// Compile-time assertion that S3Adapter satisfies hub.S3Client.
var _ S3Client = (*S3Adapter)(nil)

// NewS3Client builds a production S3Client against an R2/S3-compatible endpoint
// (P5-HUB-01). endpoint is the S3 API URL (e.g.
// https://<account>.r2.cloudflarestorage.com); region defaults to "auto" (R2);
// bucket is the target bucket. accessKeyID/secretAccessKey are the bucket-scoped
// static credentials (self-hosted mode). The SDK retryer is disabled so R2Hub.Retry
// is the single retry layer.
func NewS3Client(endpoint, region, bucket, accessKeyID, secretAccessKey string) (*S3Adapter, error) {
	switch {
	case bucket == "":
		return nil, errors.New("s3 hub: bucket is required")
	case endpoint == "":
		return nil, errors.New("s3 hub: endpoint is required")
	case accessKeyID == "" || secretAccessKey == "":
		return nil, errors.New("s3 hub: access key id and secret access key are required")
	}
	if region == "" {
		region = "auto"
	}
	creds := aws.Credentials{AccessKeyID: accessKeyID, SecretAccessKey: secretAccessKey}
	client := s3.New(s3.Options{
		Region:       region,
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		Retryer:      aws.NopRetryer{},
		Credentials:  aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) { return creds, nil }),
	})
	return &S3Adapter{client: client, bucket: bucket}, nil
}

// PutObject stores body at key. When ifNoneMatch is true the put is conditional
// on the object not already existing (If-None-Match: *), making event append and
// content-addressed blob put idempotent (HUB-06/HUB-09). A collision surfaces as
// ErrPreconditionFailed, which R2Hub classifies as a dedup no-op.
func (a *S3Adapter) PutObject(ctx context.Context, key string, body []byte, ifNoneMatch bool) error {
	in := &s3.PutObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	}
	if ifNoneMatch {
		in.IfNoneMatch = aws.String("*")
	}
	if _, err := a.client.PutObject(ctx, in); err != nil {
		return mapS3Error(err)
	}
	return nil
}

// GetObject returns the object bytes at key, or an error wrapping
// dssync.ErrBlobNotFound when the object is absent (HUB-02).
func (a *S3Adapter) GetObject(ctx context.Context, key string) (data []byte, err error) {
	out, gerr := a.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(key),
	})
	if gerr != nil {
		return nil, mapS3Error(gerr)
	}
	// bodyclose + errcheck: close on every return path and surface a close
	// error unless an earlier error already wins.
	defer func() {
		if cerr := out.Body.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close object body %s: %w", key, cerr)
		}
	}()
	data, rerr := io.ReadAll(out.Body)
	if rerr != nil {
		return nil, fmt.Errorf("read object %s: %w", key, rerr)
	}
	return data, nil
}

// ObjectExists reports whether an object exists at key via HEAD (HUB-02). A
// missing object is (false, nil), not an error.
func (a *S3Adapter) ObjectExists(ctx context.Context, key string) (bool, error) {
	_, err := a.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return true, nil
	}
	// HeadObject returns *types.NotFound (or a 404 response error) for a missing
	// object; both are a clean "does not exist".
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return false, nil
	}
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return false, nil
	}
	var respErr *http.ResponseError
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() == 404 {
		return false, nil
	}
	return false, mapS3Error(err)
}

// DeleteObject removes the object at key. A missing object is not an error
// (idempotent delete) so blob/event GC (HUB-12) and revoke cleanup (SEC-01) can
// call it unconditionally for superseded ciphertext.
func (a *S3Adapter) DeleteObject(ctx context.Context, key string) error {
	_, err := a.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return nil
	}
	mapped := mapS3Error(err)
	if errors.Is(mapped, dssync.ErrBlobNotFound) {
		return nil // idempotent: deleting a missing object is a no-op
	}
	return mapped
}

// StatObject returns one object's metadata (Key + LastModified) via HEAD, for
// hub GC's pre-delete revalidation (P4-HUB-12). A missing object wraps
// dssync.ErrBlobNotFound (via mapS3Error, which maps *types.NotFound / 404).
func (a *S3Adapter) StatObject(ctx context.Context, key string) (dssync.BlobInfo, error) {
	out, err := a.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return dssync.BlobInfo{}, mapS3Error(err)
	}
	info := dssync.BlobInfo{Key: key}
	if out.LastModified != nil {
		info.LastModified = *out.LastModified
	}
	return info, nil
}

// GetObjectWithETag returns the object bytes at key plus the object's ETag,
// for compare-and-swap read-modify-write of the retention manifest
// (P4-SYNC-02/P6-HUB-04).
func (a *S3Adapter) GetObjectWithETag(ctx context.Context, key string) (data []byte, etag string, err error) {
	out, gerr := a.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(a.bucket),
		Key:    aws.String(key),
	})
	if gerr != nil {
		return nil, "", mapS3Error(gerr)
	}
	defer func() {
		if cerr := out.Body.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close object body %s: %w", key, cerr)
		}
	}()
	data, rerr := io.ReadAll(out.Body)
	if rerr != nil {
		return nil, "", fmt.Errorf("read object %s: %w", key, rerr)
	}
	if out.ETag != nil {
		etag = *out.ETag
	}
	return data, etag, nil
}

// PutObjectIfMatch stores body at key conditionally on the current object
// still carrying etag (If-Match — an S3 extension R2 supports on PUT). A lost
// CAS race surfaces as ErrPreconditionFailed via mapS3Error.
func (a *S3Adapter) PutObjectIfMatch(ctx context.Context, key string, body []byte, etag string) error {
	in := &s3.PutObjectInput{
		Bucket:  aws.String(a.bucket),
		Key:     aws.String(key),
		Body:    bytes.NewReader(body),
		IfMatch: aws.String(etag),
	}
	if _, err := a.client.PutObject(ctx, in); err != nil {
		return mapS3Error(err)
	}
	return nil
}

// ListObjectsV2 returns objects under prefix, lexicographically after
// startAfter, up to maxKeys. When truncated, it returns the last key of the page
// as nextStartAfter (the memS3 start-after contract — NOT the S3 continuation
// token) so R2Hub.Pull/ListBlobs page with the same semantics as the in-memory
// conformance double (HUB-06).
func (a *S3Adapter) ListObjectsV2(ctx context.Context, prefix, startAfter string, maxKeys int) ([]dssync.BlobInfo, string, error) {
	if maxKeys < 1 {
		maxKeys = 1
	}
	if maxKeys > 1000 {
		maxKeys = 1000
	}
	in := &s3.ListObjectsV2Input{
		Bucket: aws.String(a.bucket),
		Prefix: aws.String(prefix),
	}
	if startAfter != "" {
		in.StartAfter = aws.String(startAfter)
	}
	// G115 (int->int32 narrowing): maxKeys is clamped to [1,1000] above, so the
	// narrowing cannot overflow or wrap.
	//nolint:gosec // G115: maxKeys clamped to [1,1000] before narrowing.
	maxKeys32 := int32(maxKeys)
	in.MaxKeys = aws.Int32(maxKeys32)
	out, err := a.client.ListObjectsV2(ctx, in)
	if err != nil {
		return nil, "", mapS3Error(err)
	}
	objs := make([]dssync.BlobInfo, 0, len(out.Contents))
	for _, obj := range out.Contents {
		if obj.Key != nil {
			info := dssync.BlobInfo{Key: *obj.Key}
			if obj.LastModified != nil {
				info.LastModified = *obj.LastModified
			}
			objs = append(objs, info)
		}
	}
	next := ""
	if out.IsTruncated != nil && *out.IsTruncated && len(objs) > 0 {
		// Resume start-after the last returned key (memS3 contract), not via the
		// continuation token, so the production adapter pages identically to the
		// in-memory conformance double.
		next = objs[len(objs)-1].Key
	}
	return objs, next, nil
}

// ListCommonPrefixes returns the distinct sub-prefixes directly under prefix,
// grouped at delimiter (P5-SYNC-01 device-stream discovery). Pagination uses
// the S3 continuation token, NOT the start-after-last-prefix trick: resuming
// after a common prefix would re-list that prefix's own keys (they sort after
// the bare prefix) and return it again — duplicate device streams past 1000
// devices (post-#59 opus review, Minor).
func (a *S3Adapter) ListCommonPrefixes(ctx context.Context, prefix, delimiter string) ([]string, error) {
	var out []string
	var token *string
	for {
		in := &s3.ListObjectsV2Input{
			Bucket:            aws.String(a.bucket),
			Prefix:            aws.String(prefix),
			Delimiter:         aws.String(delimiter),
			MaxKeys:           aws.Int32(1000),
			ContinuationToken: token,
		}
		resp, err := a.client.ListObjectsV2(ctx, in)
		if err != nil {
			return nil, mapS3Error(err)
		}
		for _, cp := range resp.CommonPrefixes {
			if cp.Prefix != nil {
				out = append(out, *cp.Prefix)
			}
		}
		if resp.IsTruncated == nil || !*resp.IsTruncated || resp.NextContinuationToken == nil {
			return out, nil
		}
		token = resp.NextContinuationToken
	}
}

// mapS3Error is the pure, load-bearing translation from aws-sdk-go-v2 errors
// into the hub sentinels (P5-HUB-01 / HUB-10 / P6-HUB-02). Classification:
//
//	412 / PreconditionFailed (R2 10031 -> 412)        -> ErrPreconditionFailed (terminal dedup)
//	*types.NoSuchKey / *types.NotFound / 404          -> dssync.ErrBlobNotFound  (terminal)
//	429 / 503 / SlowDown / TooManyRequests            -> ErrS3Throttle           (throttle)
//	500 / 502 / 504 / InternalError                   -> ErrS3Transient          (transient)
//	no modeled APIError in the chain (net/EOF/reset)  -> ErrS3Transient          (transient)
//	401 / 403 / AccessDenied /                        -> ErrS3Auth               (terminal + hint)
//	SignatureDoesNotMatch / InvalidAccessKeyId
//	NoSuchBucket / other API error                    -> raw wrapped SDK error   (terminal)
//
// Precondition + not-found are checked before the transport-error fallback so a
// dropped connection (no APIError in the chain) is retried as transient.
func mapS3Error(err error) error {
	if err == nil {
		return nil
	}
	// Not-found: modeled concrete types first (GetObject -> NoSuchKey,
	// HeadObject -> NotFound).
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return fmt.Errorf("%w: %w", dssync.ErrBlobNotFound, err)
	}
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return fmt.Errorf("%w: %w", dssync.ErrBlobNotFound, err)
	}

	var apiErr smithy.APIError
	hasAPI := errors.As(err, &apiErr)
	var respErr *http.ResponseError
	hasResp := errors.As(err, &respErr)
	status := 0
	if hasResp {
		status = respErr.HTTPStatusCode()
	}
	code := ""
	if hasAPI {
		code = apiErr.ErrorCode()
	}

	switch {
	case status == 404:
		return fmt.Errorf("%w: %w", dssync.ErrBlobNotFound, err)
	case status == 412 || code == "PreconditionFailed":
		return fmt.Errorf("%w: %w", ErrPreconditionFailed, err)
	case status == 429 || status == 503 || code == "SlowDown" || code == "TooManyRequests":
		return fmt.Errorf("%w: %w", ErrS3Throttle, err)
	case status == 500 || status == 502 || status == 504 || code == "InternalError":
		return fmt.Errorf("%w: %w", ErrS3Transient, err)
	case status == 401 || status == 403 || code == "AccessDenied" || code == "SignatureDoesNotMatch" || code == "InvalidAccessKeyId":
		// P6-HUB-02: credential failures carry an actionable hint instead of
		// surfacing as an opaque SDK error. Terminal — R2Hub.Retry only
		// retries the throttle/transient sentinels.
		return fmt.Errorf("%w (check DEVSTRAP_HUB_S3_ACCESS_KEY_ID/DEVSTRAP_HUB_S3_SECRET_ACCESS_KEY — values may be op:// refs — or store credentials once with 'devstrap hub login'): %w", ErrS3Auth, err)
	case !hasAPI:
		// No modeled API error anywhere in the chain: a transport-level failure
		// (EOF, connection reset, dial/refused) — retry as transient.
		return fmt.Errorf("%w: %w", ErrS3Transient, err)
	default:
		// Other API error (auth, NoSuchBucket, malformed, ...): terminal; surface
		// the raw SDK error so it is not retried and the caller sees the cause.
		return err
	}
}
