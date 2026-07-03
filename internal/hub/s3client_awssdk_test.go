package hub

import (
	"context"
	"errors"
	"strings"
	"testing"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smtransport "github.com/aws/smithy-go/transport/http"
	nethttp "net/http"

	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

// sdkRespErr builds a realistic aws-sdk-go-v2 *http.ResponseError carrying a
// given HTTP status and optional inner API error, so mapS3Error's status-based
// branches can be exercised hermetically (no network).
func sdkRespErr(status int, inner error) *awshttp.ResponseError {
	return &awshttp.ResponseError{
		ResponseError: &smtransport.ResponseError{
			Response: &smtransport.Response{Response: &nethttp.Response{StatusCode: status}},
			Err:      inner,
		},
	}
}

// opErr wraps an inner error in a smithy.OperationError, mirroring the SDK's
// outermost error layer so errors.As walks the same chain it does in production.
func opErr(name string, inner error) error {
	return &smithy.OperationError{ServiceID: "S3", OperationName: name, Err: inner}
}

func TestMapS3ErrorNil(t *testing.T) {
	if err := mapS3Error(nil); err != nil {
		t.Fatalf("mapS3Error(nil) = %v, want nil", err)
	}
}

func TestMapS3ErrorNoSuchKey(t *testing.T) {
	err := opErr("GetObject", &types.NoSuchKey{})
	if !errors.Is(mapS3Error(err), dssync.ErrBlobNotFound) {
		t.Fatalf("mapS3Error NoSuchKey = %v, want ErrBlobNotFound", mapS3Error(err))
	}
}

func TestMapS3ErrorNotFound(t *testing.T) {
	err := opErr("HeadObject", &types.NotFound{})
	if !errors.Is(mapS3Error(err), dssync.ErrBlobNotFound) {
		t.Fatalf("mapS3Error NotFound = %v, want ErrBlobNotFound", mapS3Error(err))
	}
}

func TestMapS3ErrorStatus404(t *testing.T) {
	// A 404 with no modeled API error still resolves to not-found (checked before
	// the transient "no APIError" fallback).
	err := opErr("GetObject", sdkRespErr(404, nil))
	if !errors.Is(mapS3Error(err), dssync.ErrBlobNotFound) {
		t.Fatalf("mapS3Error 404 = %v, want ErrBlobNotFound", mapS3Error(err))
	}
}

func TestMapS3ErrorPreconditionFailedCode(t *testing.T) {
	err := opErr("PutObject", &smithy.GenericAPIError{Code: "PreconditionFailed"})
	if !errors.Is(mapS3Error(err), ErrPreconditionFailed) {
		t.Fatalf("mapS3Error PreconditionFailed = %v, want ErrPreconditionFailed", mapS3Error(err))
	}
}

func TestMapS3ErrorStatus412(t *testing.T) {
	// R2 maps conditional-put collision (10031) to HTTP 412.
	err := opErr("PutObject", sdkRespErr(412, nil))
	if !errors.Is(mapS3Error(err), ErrPreconditionFailed) {
		t.Fatalf("mapS3Error 412 = %v, want ErrPreconditionFailed", mapS3Error(err))
	}
}

func TestMapS3ErrorThrottleCodes(t *testing.T) {
	codes := []string{"SlowDown", "TooManyRequests"}
	for _, c := range codes {
		err := opErr("PutObject", &smithy.GenericAPIError{Code: c})
		if !errors.Is(mapS3Error(err), ErrS3Throttle) {
			t.Fatalf("mapS3Error %s = %v, want ErrS3Throttle", c, mapS3Error(err))
		}
	}
}

func TestMapS3ErrorThrottleStatuses(t *testing.T) {
	for _, s := range []int{429, 503} {
		err := opErr("PutObject", sdkRespErr(s, nil))
		if !errors.Is(mapS3Error(err), ErrS3Throttle) {
			t.Fatalf("mapS3Error %d = %v, want ErrS3Throttle", s, mapS3Error(err))
		}
	}
}

func TestMapS3ErrorInternalErrorCode(t *testing.T) {
	err := opErr("GetObject", &smithy.GenericAPIError{Code: "InternalError"})
	if !errors.Is(mapS3Error(err), ErrS3Transient) {
		t.Fatalf("mapS3Error InternalError = %v, want ErrS3Transient", mapS3Error(err))
	}
}

func TestMapS3ErrorTransientStatuses(t *testing.T) {
	for _, s := range []int{500, 502, 504} {
		err := opErr("PutObject", sdkRespErr(s, nil))
		if !errors.Is(mapS3Error(err), ErrS3Transient) {
			t.Fatalf("mapS3Error %d = %v, want ErrS3Transient", s, mapS3Error(err))
		}
	}
}

func TestMapS3ErrorNoAPITransient(t *testing.T) {
	// A plain transport/network error with no modeled APIError in the chain
	// (EOF, connection reset, dial refused) retries as transient.
	err := opErr("PutObject", errors.New("dial tcp: connection reset by peer"))
	if !errors.Is(mapS3Error(err), ErrS3Transient) {
		t.Fatalf("mapS3Error net error = %v, want ErrS3Transient", mapS3Error(err))
	}
}

func TestMapS3ErrorOtherAPITerminal(t *testing.T) {
	// An auth/permissions API error is terminal: not any retry sentinel, and the
	// raw SDK error is surfaced so the caller sees the cause.
	inner := &smithy.GenericAPIError{Code: "AccessDenied", Message: "denied"}
	err := opErr("PutObject", inner)
	got := mapS3Error(err)
	if errors.Is(got, ErrPreconditionFailed) || errors.Is(got, ErrS3Throttle) ||
		errors.Is(got, ErrS3Transient) || errors.Is(got, dssync.ErrBlobNotFound) {
		t.Fatalf("mapS3Error AccessDenied = %v, want terminal (no retry sentinel)", got)
	}
	if !errors.Is(got, err) {
		t.Fatalf("mapS3Error AccessDenied = %v, want the original SDK error surfaced", got)
	}
}

func TestNewS3ClientValidation(t *testing.T) {
	ctx := context.Background()
	_ = ctx
	cases := []struct {
		name                                     string
		endpoint, region, bucket, access, secret string
	}{
		{"empty bucket", "http://x", "auto", "", "k", "s"},
		{"empty endpoint", "", "auto", "b", "k", "s"},
		{"empty access key", "http://x", "auto", "b", "", "s"},
		{"empty secret key", "http://x", "auto", "b", "k", ""},
	}
	for _, c := range cases {
		if _, err := NewS3Client(c.endpoint, c.region, c.bucket, c.access, c.secret); err == nil {
			t.Fatalf("NewS3Client(%s): want error, got nil", c.name)
		}
	}
	// A valid construction does not perform any network I/O (s3.New only assembles
	// the client), so it is safe to exercise hermetically. region defaults to auto.
	ad, err := NewS3Client("http://localhost:9000", "", "devstrap-test", "k", "s")
	if err != nil {
		t.Fatalf("NewS3Client valid: %v", err)
	}
	if ad == nil || ad.bucket != "devstrap-test" {
		t.Fatalf("NewS3Client valid = %+v, want non-nil adapter for devstrap-test", ad)
	}
}

// P6-HUB-02: credential failures map to ErrS3Auth with an actionable hint
// (previously they fell through to the raw SDK error — an opaque
// SignatureDoesNotMatch when an op:// ref was signed as the literal secret).
func TestMapS3ErrorAuthCodes(t *testing.T) {
	for _, c := range []string{"AccessDenied", "SignatureDoesNotMatch", "InvalidAccessKeyId"} {
		mapped := mapS3Error(opErr("PutObject", &smithy.GenericAPIError{Code: c}))
		if !errors.Is(mapped, ErrS3Auth) {
			t.Fatalf("mapS3Error %s = %v, want ErrS3Auth", c, mapped)
		}
		if !strings.Contains(mapped.Error(), "hub login") {
			t.Fatalf("mapS3Error %s = %q, want remediation hint", c, mapped)
		}
	}
}

func TestMapS3ErrorAuthStatuses(t *testing.T) {
	for _, status := range []int{401, 403} {
		mapped := mapS3Error(opErr("PutObject", sdkRespErr(status, nil)))
		if !errors.Is(mapped, ErrS3Auth) {
			t.Fatalf("mapS3Error %d = %v, want ErrS3Auth", status, mapped)
		}
	}
}
