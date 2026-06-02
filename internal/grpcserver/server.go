// Package grpcserver provides a gRPC endpoint for querying and controlling
// nomad-botherer. All RPCs require a pre-shared API key supplied in the
// "authorization" metadata header as "Bearer <key>".
package grpcserver

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/gerrowadat/nomad-botherer/internal/grpcapi"
	"github.com/gerrowadat/nomad-botherer/internal/nomad"
)

// DiffSource is satisfied by *nomad.Differ.
type DiffSource interface {
	Diffs() ([]nomad.JobDiff, time.Time, string)
	SelectedJobs() ([]nomad.SelectedJob, time.Time, string)
	// Ready reports whether at least one diff check has completed.
	Ready() bool
}

// GitStatusSource is satisfied by *gitwatch.Watcher.
type GitStatusSource interface {
	Trigger()
	Status() (lastCommit string, lastUpdate time.Time)
	// Ready reports whether the initial git clone has completed.
	Ready() bool
}

// BuildInfo holds the version strings injected at link time.
type BuildInfo struct {
	Version   string
	Commit    string
	BuildDate string
}

// Server implements grpcapi.NomadBothererServer.
type Server struct {
	grpcapi.UnimplementedNomadBothererServer

	// expectedToken is pre-computed as "Bearer <apiKey>" to avoid allocation
	// per request and to allow constant-time comparison in the interceptor.
	expectedToken string
	diffs         DiffSource
	git           GitStatusSource
	buildInfo     BuildInfo

	// Prometheus metrics
	rpcTotal  *prometheus.CounterVec
	rpcErrors *prometheus.CounterVec
}

// New creates a Server using the default Prometheus registry.
func New(apiKey string, diffs DiffSource, git GitStatusSource, info BuildInfo) *Server {
	return NewWithRegistry(apiKey, diffs, git, info, prometheus.DefaultRegisterer)
}

// NewWithRegistry creates a Server with a custom Prometheus Registerer.
func NewWithRegistry(apiKey string, diffs DiffSource, git GitStatusSource, info BuildInfo, reg prometheus.Registerer) *Server {
	return &Server{
		expectedToken: "Bearer " + apiKey,
		diffs:         diffs,
		git:           git,
		buildInfo:     info,
		rpcTotal: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "nomad_botherer_grpc_requests_total",
			Help: "Authenticated gRPC requests completed, by method and gRPC status code. Does not include requests rejected before auth — see nomad_botherer_grpc_auth_errors_total for those.",
		}, []string{"method", "code"}),
		rpcErrors: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "nomad_botherer_grpc_auth_errors_total",
			Help: "gRPC requests rejected due to a missing or invalid API key, by method.",
		}, []string{"method"}),
	}
}

// GRPCServer builds and returns a configured *grpc.Server bound to s.
// The caller is responsible for registering it on a listener and shutting it
// down gracefully.
func (s *Server) GRPCServer() *grpc.Server {
	srv := grpc.NewServer(
		grpc.UnaryInterceptor(s.authInterceptor),
	)
	grpcapi.RegisterNomadBothererServer(srv, s)
	return srv
}

// Listen binds to addr and returns a net.Listener. Call Serve to start
// accepting connections. Separating the two steps lets callers fail fast on
// a bind error before detaching into a goroutine.
func (s *Server) Listen(addr string) (net.Listener, error) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("grpc listen %s: %w", addr, err)
	}
	return lis, nil
}

// Serve starts accepting gRPC connections on lis and blocks until ctx is
// cancelled, at which point it drains in-flight requests via GracefulStop.
func (s *Server) Serve(ctx context.Context, lis net.Listener) error {
	srv := s.GRPCServer()

	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()

	slog.Info("gRPC server listening", "addr", lis.Addr())
	if err := srv.Serve(lis); err != nil && err != grpc.ErrServerStopped {
		return fmt.Errorf("grpc serve: %w", err)
	}
	return nil
}

// authInterceptor enforces the pre-shared API key.
// Clients must supply metadata: authorization: Bearer <key>
func (s *Server) authInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		s.rpcErrors.WithLabelValues(info.FullMethod).Inc()
		return nil, status.Error(codes.Unauthenticated, "missing metadata")
	}

	values := md.Get("authorization")
	if len(values) == 0 || subtle.ConstantTimeCompare([]byte(values[0]), []byte(s.expectedToken)) != 1 {
		s.rpcErrors.WithLabelValues(info.FullMethod).Inc()
		return nil, status.Error(codes.Unauthenticated, "invalid or missing API key")
	}

	resp, err := handler(ctx, req)
	code := codes.OK
	if err != nil {
		if st, ok := status.FromError(err); ok {
			code = st.Code()
		} else {
			code = codes.Unknown
		}
	}
	s.rpcTotal.WithLabelValues(info.FullMethod, code.String()).Inc()
	return resp, err
}

// GetDiffs returns the latest set of job diffs.
func (s *Server) GetDiffs(_ context.Context, _ *grpcapi.GetDiffsRequest) (*grpcapi.GetDiffsResponse, error) {
	if !s.git.Ready() || !s.diffs.Ready() {
		return nil, status.Error(codes.Unavailable, "server is not ready: initial state not yet built")
	}
	diffs, lastCheck, lastCommit := s.diffs.Diffs()

	pbDiffs := make([]*grpcapi.JobDiff, 0, len(diffs))
	for _, d := range diffs {
		pbDiffs = append(pbDiffs, &grpcapi.JobDiff{
			JobId:    d.JobID,
			HclFile:  d.HCLFile,
			DiffType: string(d.DiffType),
			Detail:   d.Detail,
		})
	}

	resp := &grpcapi.GetDiffsResponse{
		Diffs:      pbDiffs,
		LastCommit: lastCommit,
	}
	if !lastCheck.IsZero() {
		resp.LastCheckTime = lastCheck.UTC().Format(time.RFC3339)
	}
	return resp, nil
}

// GetSelectedJobs returns the jobs that matched the configured selection criteria
// during the last check, together with the reason each was included.
func (s *Server) GetSelectedJobs(_ context.Context, _ *grpcapi.GetSelectedJobsRequest) (*grpcapi.GetSelectedJobsResponse, error) {
	if !s.git.Ready() || !s.diffs.Ready() {
		return nil, status.Error(codes.Unavailable, "server is not ready: initial state not yet built")
	}
	jobs, lastCheck, lastCommit := s.diffs.SelectedJobs()

	pbJobs := make([]*grpcapi.SelectedJob, 0, len(jobs))
	for _, j := range jobs {
		pbJobs = append(pbJobs, &grpcapi.SelectedJob{
			JobId:           j.JobID,
			SelectionReason: string(j.Reason),
		})
	}

	resp := &grpcapi.GetSelectedJobsResponse{
		Jobs:       pbJobs,
		LastCommit: lastCommit,
	}
	if !lastCheck.IsZero() {
		resp.LastCheckTime = lastCheck.UTC().Format(time.RFC3339)
	}
	return resp, nil
}

// GetStatus returns git watcher status.
func (s *Server) GetStatus(_ context.Context, _ *grpcapi.GetStatusRequest) (*grpcapi.GetStatusResponse, error) {
	if !s.git.Ready() {
		return nil, status.Error(codes.Unavailable, "server is not ready: git state not yet initialized")
	}
	lastCommit, lastUpdate := s.git.Status()
	resp := &grpcapi.GetStatusResponse{
		LastCommit: lastCommit,
	}
	if !lastUpdate.IsZero() {
		resp.LastUpdateTime = lastUpdate.UTC().Format(time.RFC3339)
	}
	return resp, nil
}

// TriggerRefresh triggers an immediate git pull.
func (s *Server) TriggerRefresh(_ context.Context, _ *grpcapi.TriggerRefreshRequest) (*grpcapi.TriggerRefreshResponse, error) {
	s.git.Trigger()
	return &grpcapi.TriggerRefreshResponse{Message: "refresh triggered"}, nil
}

// GetVersion returns the build version, commit, and build date.
func (s *Server) GetVersion(_ context.Context, _ *grpcapi.GetVersionRequest) (*grpcapi.GetVersionResponse, error) {
	return &grpcapi.GetVersionResponse{
		Version:   s.buildInfo.Version,
		Commit:    s.buildInfo.Commit,
		BuildDate: s.buildInfo.BuildDate,
	}, nil
}
