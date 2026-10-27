package grpcx

import (
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xshubhamg/chore-microservices/internal/platform/apperr"
)

// ToStatus converts the shared application error into a gRPC status without
// leaking internal causes over the wire.
func ToStatus(err error) error {
	if err == nil {
		return nil
	}

	appErr := apperr.From(err)
	return status.Error(grpcCode(appErr.Kind), appErr.Message)
}

// FromStatus converts a gRPC error returned by a dependency into the shared
// application error, so callers never have to import google.golang.org/grpc.
func FromStatus(err error) error {
	if err == nil {
		return nil
	}

	st, ok := status.FromError(err)
	if !ok {
		return apperr.Internal(err)
	}

	switch st.Code() {
	case codes.NotFound:
		return apperr.New(apperr.KindNotFound, "not_found", st.Message())
	case codes.InvalidArgument:
		return apperr.New(apperr.KindInvalid, "invalid_argument", st.Message())
	case codes.AlreadyExists:
		return apperr.New(apperr.KindConflict, "conflict", st.Message())
	case codes.PermissionDenied, codes.Unauthenticated:
		return apperr.New(apperr.KindForbidden, "permission_denied", st.Message())
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted:
		return apperr.New(apperr.KindUnavailable, "upstream_unavailable", "catalog service is unavailable")
	default:
		var fallback *apperr.Error
		if errors.As(err, &fallback) {
			return fallback
		}
		return apperr.New(apperr.KindInternal, "upstream_error", "catalog service returned an error")
	}
}

func grpcCode(kind apperr.Kind) codes.Code {
	switch kind {
	case apperr.KindInvalid:
		return codes.InvalidArgument
	case apperr.KindUnauthenticated:
		return codes.Unauthenticated
	case apperr.KindForbidden:
		return codes.PermissionDenied
	case apperr.KindNotFound:
		return codes.NotFound
	case apperr.KindConflict:
		return codes.AlreadyExists
	case apperr.KindUnavailable:
		return codes.Unavailable
	default:
		return codes.Internal
	}
}
