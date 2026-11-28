package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coxlong/mnevo/internal/domain"
	"github.com/coxlong/mnevo/internal/store"
	"github.com/coxlong/mnevo/internal/web"
)

type transportKey struct{}

const (
	transportUnix = "unix"
	transportTCP  = "tcp"
)

func withTransport(ctx context.Context, kind string) context.Context {
	return context.WithValue(ctx, transportKey{}, kind)
}

func isUnixTransport(ctx context.Context) bool {
	v, _ := ctx.Value(transportKey{}).(string)
	return v == transportUnix
}

// Service orchestrates the unified HTTP server (serving BOTH Unix Socket API and optional
// TCP loopback Web UI), durable background workers, and typed web.Backend operations.
type Service struct {
	store       *store.Store
	homeDir     string
	version     string
	key         string
	recallDelay time.Duration
	logger      *slog.Logger

	mu       sync.RWMutex
	webURL   string
	shutdown context.CancelFunc
}

// Option customizes Service construction.
type Option func(*Service)

// WithRecallDelay injects artificial recall latency for timeout integration testing.
func WithRecallDelay(d time.Duration) Option {
	return func(s *Service) { s.recallDelay = d }
}

// WithLogger sets a custom structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithBuildInfo records version and home directory reported by GET /v1/health.
func WithBuildInfo(version, homeDir string) Option {
	return func(s *Service) {
		s.version = version
		s.homeDir = homeDir
	}
}

// NewService constructs a Service wrapping store and admin key.
func NewService(st *store.Store, adminKey string, opts ...Option) *Service {
	s := &Service{
		store:   st,
		key:     adminKey,
		version: "dev",
		logger:  slog.Default(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// WebURL returns the bound loopback management URL (empty if Web UI is disabled).
func (s *Service) WebURL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.webURL
}

// Handler returns the unified HTTP handler shared across Unix Socket (/v1/* REST API)
// and TCP loopback (Web UI at /). Transport isolation ensures TCP clients can never
// reach Unix-only Agent/Admin API routes.
func (s *Service) Handler(webOrigin string) http.Handler {
	apiMux := s.buildAPIMux()
	var webHandler http.Handler
	if webOrigin != "" {
		webHandler = web.NewHandler(s, s.key, webOrigin, s.logger)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isUnixTransport(r.Context()) {
			apiMux.ServeHTTP(w, r)
			return
		}
		if webHandler != nil {
			webHandler.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	})
}

// Serve binds the Unix socket and optional TCP loopback listener onto a SINGLE shared
// http.Server + Handler, launches background workers, and blocks until ctx is canceled
// or POST /v1/shutdown is called.
func (s *Service) Serve(ctx context.Context, socketPath, webAddr string, allowPortFallback bool) error {
	if info, err := os.Lstat(socketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("socket path is occupied by a non-socket")
		}
		if err := os.Remove(socketPath); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	unixLn, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		return err
	}
	defer func() {
		_ = unixLn.Close()
		_ = os.Remove(socketPath)
	}()
	if err := os.Chmod(socketPath, 0600); err != nil {
		return err
	}

	var tcpLn net.Listener
	var webOrigin string
	if webAddr != "" {
		host, _, splitErr := net.SplitHostPort(webAddr)
		if splitErr != nil || host != "127.0.0.1" {
			return errors.New("management address must use 127.0.0.1:PORT")
		}
		tcpLn, err = net.Listen("tcp", webAddr)
		if err != nil && allowPortFallback {
			s.logger.Warn("preferred web port unavailable; falling back to ephemeral port", "requested", webAddr, "error", err)
			tcpLn, err = net.Listen("tcp", "127.0.0.1:0")
		}
		if err != nil {
			return err
		}
		defer tcpLn.Close()
		webOrigin = "http://" + tcpLn.Addr().String()
		s.mu.Lock()
		s.webURL = webOrigin
		s.mu.Unlock()
	}

	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.shutdown = cancel
	s.mu.Unlock()
	defer cancel()

	srv := &http.Server{
		Handler:           s.Handler(webOrigin),
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      65 * time.Second,
		IdleTimeout:       60 * time.Second,
		ConnContext: func(connCtx context.Context, c net.Conn) context.Context {
			if verifyUnixPeer(c) {
				return withTransport(connCtx, transportUnix)
			}
			return withTransport(connCtx, transportTCP)
		},
	}

	var wg sync.WaitGroup
	s.startWorkers(ctx, &wg)

	errCh := make(chan error, 2)
	wg.Go(func() {
		if err := srv.Serve(unixLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	})
	if tcpLn != nil {
		s.logger.Info("management web server listening", "url", webOrigin)
		wg.Go(func() {
			if err := srv.Serve(tcpLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		})
	}

	s.logger.Info("daemon listening", "socket", socketPath, "web", webOrigin)

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errCh:
		cancel()
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
	wg.Wait()
	return serveErr
}

// HandleHook processes a Claude Code hook event (POST /v1/hooks) using SQL-backed session turns.
func (s *Service) HandleHook(ctx context.Context, req domain.HookRequest) (domain.RecallResult, error) {
	switch req.Event {
	case "Stop", "StopFailure", "SessionEnd":
		ended, err := s.store.EndTurn(ctx, req.SessionID, req.Event)
		if err != nil {
			return domain.RecallResult{}, err
		}
		if req.TranscriptPath != "" && req.Event != "StopFailure" {
			if _, err := s.store.EnqueueTranscriptDelta(ctx, ended.SessionID, ended.ID, req.TranscriptPath); err != nil {
				s.logger.Warn("transcript delta enqueue failed", "error", err)
			}
		}
		return domain.RecallResult{Status: "ok"}, nil

	case "UserPromptSubmit":
		task, err := s.store.BeginTurn(ctx, req.SessionID, req.Event)
		if err != nil {
			return domain.RecallResult{}, err
		}
		trimmed := strings.TrimSpace(req.Prompt)
		if raw, isRemember := strings.CutPrefix(trimmed, "记住："); isRemember {
			raw = strings.TrimSpace(raw)
			if strings.HasPrefix(raw, "{") {
				var draft domain.MemoryDraft
				if err := domain.DecodeDocument(strings.NewReader(raw), &draft); err != nil {
					return domain.RecallResult{Status: "invalid"}, nil
				}
				rec, err := s.store.EnqueueFormation(ctx, task, "user", "", "", &draft)
				return domain.RecallResult{Status: rec.Status, Receipt: rec.Receipt}, err
			}
			rec, err := s.store.EnqueueFormation(ctx, task, "user", "", raw, nil)
			return domain.RecallResult{Status: rec.Status, Receipt: rec.Receipt}, err
		}
		return s.runRecallWithBudget(ctx, task, req)

	default:
		return domain.RecallResult{Status: "no_match"}, nil
	}
}

func (s *Service) runRecallWithBudget(ctx context.Context, cause domain.Task, req domain.HookRequest) (domain.RecallResult, error) {
	started := time.Now()
	budget, cancelBudget := context.WithTimeout(ctx, domain.RecallBudget-50*time.Millisecond)
	defer cancelBudget()

	pol := s.store.Policy().Active()
	recallCtx, cancelRecall := context.WithDeadline(budget, started.Add(domain.Milliseconds(pol.Value("recall.deadline_ms"))))
	defer cancelRecall()

	var result domain.RecallResult
	var err error
	if s.recallDelay > 0 {
		timer := time.NewTimer(s.recallDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-recallCtx.Done():
			err = recallCtx.Err()
		}
	}
	if err == nil {
		result, err = s.store.RecallFromTranscript(recallCtx, cause, req.Prompt, req.TranscriptPath, pol)
	}
	if errors.Is(err, context.DeadlineExceeded) || (err != nil && recallCtx.Err() == context.DeadlineExceeded) {
		result = domain.RecallResult{Status: "timeout", Strategy: pol.Label()}
		s.logger.Warn("recall timed out", "session", cause.SessionID, "task", cause.ID)
		return result, s.store.Audit(ctx, nil, "recall", cause, store.AuditRecord{
			Strategy: pol.Label(), Status: "timeout",
		})
	}
	return result, err
}

func (s *Service) validAdminKey(r *http.Request) bool {
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Mnevo-Key")), []byte(s.key)) == 1
}

// --- web.Backend implementation (clean delegation to store & policy & model registry) ---

func (s *Service) ListMemories(ctx context.Context, filter, id string) ([]domain.MemoryRecord, error) {
	return s.store.ListMemories(ctx, filter, id)
}

func (s *Service) CreateMemory(ctx context.Context, d domain.MemoryDraft) (string, int, error) {
	return s.store.CreateMemory(ctx, d)
}

func (s *Service) CorrectMemory(ctx context.Context, id string, d domain.MemoryDraft) (int, bool, error) {
	return s.store.CorrectMemory(ctx, id, d)
}

func (s *Service) SetMemoryEnabled(ctx context.Context, id string, enabled bool) (bool, error) {
	return s.store.SetMemoryEnabled(ctx, id, enabled)
}

func (s *Service) SetMemoryState(ctx context.Context, id, state string) (bool, error) {
	return s.store.SetMemoryState(ctx, id, state)
}

func (s *Service) ImportDocument(ctx context.Context, in domain.ImportInput) (domain.ImportResult, error) {
	return s.store.ImportDocument(ctx, in)
}

func (s *Service) PublicURL() string {
	return s.store.Models().PublicURL()
}

func (s *Service) UpdateWebConfig(ctx context.Context, cfg domain.WebConfig) (domain.WebConfig, error) {
	saved, err := s.store.Models().UpdateWebConfig(cfg)
	if err != nil {
		return domain.WebConfig{}, err
	}
	_ = s.store.Audit(ctx, nil, "web_configure", domain.Task{}, store.AuditRecord{Status: "ok", Summary: saved.PublicURL})
	return saved, nil
}

func (s *Service) ModelConfigView(ctx context.Context) domain.ModelConfigView {
	return s.store.ModelConfigView(ctx)
}

func (s *Service) UpdateModelEndpoint(ctx context.Context, role string, in domain.ModelEndpointInput) (domain.ModelConfigView, error) {
	if err := s.store.Models().UpdateEndpoint(role, in); err != nil {
		return domain.ModelConfigView{}, err
	}
	_ = s.store.Audit(ctx, nil, "model_configure", domain.Task{}, store.AuditRecord{Status: "ok", Summary: role})
	return s.store.ModelConfigView(ctx), nil
}

func (s *Service) TestModelEndpoint(ctx context.Context, role string) (int64, error) {
	return s.store.Models().TestConnection(ctx, role)
}

func (s *Service) ActivePolicy() domain.Policy {
	return s.store.Policy().Active()
}

func (s *Service) PolicyHistory() ([]domain.PolicyRevision, error) {
	return s.store.Policy().History()
}

func (s *Service) UpdateSetting(ctx context.Context, key string, item domain.Setting) (domain.Policy, error) {
	pol, err := s.store.Policy().UpdateSetting(key, item, "local-admin", "configure "+key)
	if err != nil {
		return domain.Policy{}, err
	}
	_ = s.store.Audit(ctx, nil, "strategy_set", domain.Task{}, store.AuditRecord{Status: "ok", Strategy: pol.Label()})
	return pol, nil
}

func (s *Service) UpdatePrompt(ctx context.Context, key string, item domain.PromptSetting) (domain.Policy, error) {
	pol, err := s.store.Policy().UpdatePrompt(key, item, "local-admin", "configure prompt "+key)
	if err != nil {
		return domain.Policy{}, err
	}
	_ = s.store.Audit(ctx, nil, "strategy_set_prompt", domain.Task{}, store.AuditRecord{Status: "ok", Strategy: pol.Label()})
	return pol, nil
}

func (s *Service) RollbackPolicy(ctx context.Context, version int) (domain.Policy, error) {
	pol, err := s.store.Policy().Rollback(version, "local-admin")
	if err != nil {
		return domain.Policy{}, err
	}
	_ = s.store.Audit(ctx, nil, "strategy_rollback", domain.Task{}, store.AuditRecord{Status: "ok", Strategy: pol.Label()})
	return pol, nil
}

func (s *Service) ListProposals(ctx context.Context) ([]domain.Proposal, error) {
	return s.store.ListProposals(ctx)
}

func (s *Service) ResolveProposal(ctx context.Context, id string, approve bool) (string, domain.Policy, error) {
	return s.store.ResolveProposal(ctx, id, approve, "local-admin")
}

func (s *Service) ListObservations(ctx context.Context, id string) ([]domain.ObservationRecord, error) {
	return s.store.ListObservations(ctx, id)
}

func (s *Service) RecordObservation(ctx context.Context, o domain.Observation) (bool, error) {
	return s.store.RecordObservation(ctx, o)
}

func (s *Service) ConfirmObservation(ctx context.Context, id string) (bool, error) {
	return s.store.ConfirmObservation(ctx, id)
}

func (s *Service) AuditPage(ctx context.Context, before int64) ([]domain.AuditEntry, error) {
	return s.store.AuditPage(ctx, before)
}

// Helper for JSON responses inside handlers.go.
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, domain.MaxMessage)
	return domain.DecodeDocument(r.Body, dst)
}
