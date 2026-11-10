package remote

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"example.com/firecracker-postgres-builder/vmlib"
)

const (
	reasonRetained   = "VMLIB_RETAINED"
	reasonNotStopped = "VMLIB_NOT_STOPPED"
)

// toStatus maps domain errors to gRPC statuses. Unknown errors are not echoed
// to the caller (they may contain host paths).
func toStatus(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok && status.Code(err) != codes.Unknown {
		return err
	}
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	case errors.Is(err, vmlib.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, vmlib.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, vmlib.ErrAlreadyExists):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, vmlib.ErrResourceExhausted):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, vmlib.ErrRetained):
		return precondition(err.Error(), reasonRetained)
	case errors.Is(err, vmlib.ErrNotStopped):
		return precondition(err.Error(), reasonNotStopped)
	}
	return status.Error(codes.Internal, "internal error; see server logs")
}

func precondition(msg, reason string) error {
	st := status.New(codes.FailedPrecondition, msg)
	if d, err := st.WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: "vmlib"}); err == nil {
		return d.Err()
	}
	return st.Err()
}

type remoteError struct {
	msg    string
	target error
}

func (e *remoteError) Error() string { return e.msg }
func (e *remoteError) Unwrap() error { return e.target }

// fromStatus maps a gRPC error back to vmlib sentinels so errors.Is works the
// same for local and remote callers.
func fromStatus(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	var target error
	switch st.Code() {
	case codes.InvalidArgument:
		target = vmlib.ErrInvalidArgument
	case codes.NotFound:
		target = vmlib.ErrNotFound
	case codes.AlreadyExists:
		target = vmlib.ErrAlreadyExists
	case codes.ResourceExhausted:
		target = vmlib.ErrResourceExhausted
	case codes.Canceled:
		target = context.Canceled
	case codes.DeadlineExceeded:
		target = context.DeadlineExceeded
	case codes.FailedPrecondition:
		target = vmlib.ErrNotStopped
		for _, d := range st.Details() {
			if info, ok := d.(*errdetails.ErrorInfo); ok && info.Reason == reasonRetained {
				target = vmlib.ErrRetained
			}
		}
	default:
		return fmt.Errorf("remote %s: %s", st.Code(), st.Message())
	}
	return &remoteError{msg: st.Message(), target: target}
}

// Limits applied to every server: small messages, bounded streams.
func serverOptions(cfg *tls.Config) []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.Creds(credentials.NewTLS(cfg)),
		grpc.MaxRecvMsgSize(64 << 10),
		grpc.MaxConcurrentStreams(64),
	}
}

// NewGRPCServer returns a gRPC server that only speaks the given TLS config.
// Pass a config from ServerTLSFromPEM/Files; it must require client certs.
func NewGRPCServer(cfg *tls.Config) *grpc.Server {
	if cfg == nil || cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		panic("remote: server TLS config must require and verify client certificates")
	}
	return grpc.NewServer(serverOptions(cfg)...)
}

func dialOptions(cfg *tls.Config) ([]grpc.DialOption, error) {
	if cfg == nil || cfg.InsecureSkipVerify || cfg.ServerName == "" || cfg.RootCAs == nil {
		return nil, errors.New("client TLS config must verify the server chain and hostname")
	}
	return []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(cfg)), grpc.WithMaxHeaderListSize(16 << 10)}, nil
}
