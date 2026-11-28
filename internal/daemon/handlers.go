package daemon

import (
	"net/http"
	"os"
	"strconv"

	"github.com/coxlong/mnevo/internal/domain"
)

// buildAPIMux registers all Unix-socket REST endpoints on Go's standard http.ServeMux.
func (s *Service) buildAPIMux() *http.ServeMux {
	mux := http.NewServeMux()
	admin := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !s.validAdminKey(r) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"status": "unauthorized"})
				return
			}
			h(w, r)
		}
	}

	// Health & lifecycle
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("POST /v1/shutdown", admin(s.handleShutdown))

	// Agent Hook & MCP endpoints (strictly Unix-socket same-UID)
	mux.HandleFunc("POST /v1/hooks", s.handleHook)
	mux.HandleFunc("GET /v1/references/{ref}", s.handleGetReference)
	mux.HandleFunc("POST /v1/references/{ref}/feedback", s.handleSubmitFeedback)
	mux.HandleFunc("POST /v1/memories/propose", s.handleProposeMemory)
	mux.HandleFunc("POST /v1/observations/miss", s.handleReportMiss)

	// Admin Memory endpoints
	mux.HandleFunc("GET /v1/memories", admin(s.handleListMemories))
	mux.HandleFunc("POST /v1/memories", admin(s.handleCreateMemory))
	mux.HandleFunc("POST /v1/memories/import", admin(s.handleImportMemories))
	mux.HandleFunc("GET /v1/memories/{id}", admin(s.handleShowMemory))
	mux.HandleFunc("PUT /v1/memories/{id}", admin(s.handleCorrectMemory))
	mux.HandleFunc("DELETE /v1/memories/{id}", admin(s.handleDeleteMemory))
	mux.HandleFunc("POST /v1/memories/{id}/{action}", admin(s.handleMemoryAction))

	// Admin Model & Web configuration endpoints
	mux.HandleFunc("GET /v1/models", admin(s.handleGetModels))
	mux.HandleFunc("PUT /v1/models/{role}", admin(s.handleUpdateModel))
	mux.HandleFunc("POST /v1/models/{role}/test", admin(s.handleTestModel))
	mux.HandleFunc("PUT /v1/web", admin(s.handleUpdateWeb))

	// Admin Strategy, Proposals, Observations & Audit endpoints
	mux.HandleFunc("GET /v1/strategy", admin(s.handleGetStrategy))
	mux.HandleFunc("GET /v1/strategy/history", admin(s.handleStrategyHistory))
	mux.HandleFunc("PUT /v1/strategy/settings/{key}", admin(s.handleUpdateSetting))
	mux.HandleFunc("PUT /v1/strategy/prompts/{key}", admin(s.handleUpdatePrompt))
	mux.HandleFunc("POST /v1/strategy/rollback", admin(s.handleRollback))
	mux.HandleFunc("GET /v1/proposals", admin(s.handleListProposals))
	mux.HandleFunc("POST /v1/proposals/{id}/{decision}", admin(s.handleResolveProposal))
	mux.HandleFunc("GET /v1/observations", admin(s.handleListObservations))
	mux.HandleFunc("POST /v1/observations", admin(s.handleRecordObservation))
	mux.HandleFunc("POST /v1/observations/{id}/confirm", admin(s.handleConfirmObservation))
	mux.HandleFunc("GET /v1/audit", admin(s.handleAudit))

	return mux
}

func (s *Service) handleHealth(w http.ResponseWriter, r *http.Request) {
	mc := s.ModelConfigView(r.Context())
	writeJSON(w, http.StatusOK, domain.HealthStatus{
		Status:           "ok",
		Version:          s.version,
		PID:              os.Getpid(),
		HomeDir:          s.homeDir,
		WebURL:           s.WebURL(),
		PublicURL:        s.PublicURL(),
		RecallReady:      mc.RecallReady,
		AutoExtractReady: mc.AutoExtractReady,
		EmbeddedMemories: mc.EmbeddedMemories,
		TotalMemories:    mc.TotalMemories,
	})
}

func (s *Service) handleShutdown(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopping"})
	s.mu.RLock()
	cancel := s.shutdown
	s.mu.RUnlock()
	if cancel != nil {
		go cancel()
	}
}

func (s *Service) handleHook(w http.ResponseWriter, r *http.Request) {
	var req domain.HookRequest
	if err := readJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, domain.RecallResult{Status: "invalid"})
		return
	}
	res, err := s.HandleHook(r.Context(), req)
	if err != nil {
		s.logger.Error("hook error", "event", req.Event, "error", err)
		writeJSON(w, http.StatusInternalServerError, domain.RecallResult{Status: "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Service) handleGetReference(w http.ResponseWriter, r *http.Request) {
	detail, err := s.store.GetReference(r.Context(), r.PathValue("ref"))
	if err != nil {
		s.logger.Error("get_memory error", "error", err)
		writeJSON(w, http.StatusInternalServerError, domain.MemoryDetail{Status: "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Service) handleSubmitFeedback(w http.ResponseWriter, r *http.Request) {
	var in domain.FeedbackInput
	if err := readJSON(w, r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, domain.ReceiptResult{Status: "invalid"})
		return
	}
	in.Reference = r.PathValue("ref")
	res, err := s.store.SubmitFeedback(r.Context(), in)
	if err != nil {
		s.logger.Error("submit_feedback error", "error", err)
		writeJSON(w, http.StatusInternalServerError, domain.ReceiptResult{Status: "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Service) handleProposeMemory(w http.ResponseWriter, r *http.Request) {
	var d domain.MemoryDraft
	if err := readJSON(w, r, &d); err != nil {
		writeJSON(w, http.StatusBadRequest, domain.ReceiptResult{Status: "invalid"})
		return
	}
	res, err := s.store.EnqueueFormation(r.Context(), domain.Task{}, "agent", "", "", &d)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, domain.ReceiptResult{Status: "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Service) handleReportMiss(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Evidence string `json:"evidence"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, domain.ReceiptResult{Status: "invalid"})
		return
	}
	ok, err := s.store.ReportMiss(r.Context(), body.Evidence)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, domain.ReceiptResult{Status: "unavailable"})
		return
	}
	status := "unavailable"
	if ok {
		status = "accepted"
	}
	writeJSON(w, http.StatusOK, domain.ReceiptResult{Status: status})
}

func (s *Service) handleListMemories(w http.ResponseWriter, r *http.Request) {
	rows, err := s.ListMemories(r.Context(), r.URL.Query().Get("q"), "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"status": "error", "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "memories": rows})
}

func (s *Service) handleShowMemory(w http.ResponseWriter, r *http.Request) {
	rows, err := s.ListMemories(r.Context(), "", r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"status": "error", "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "memories": rows})
}

func (s *Service) handleCreateMemory(w http.ResponseWriter, r *http.Request) {
	var d domain.MemoryDraft
	if err := readJSON(w, r, &d); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "invalid", "message": err.Error()})
		return
	}
	id, ver, err := s.CreateMemory(r.Context(), d)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "invalid", "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "id": id, "version": ver})
}

func (s *Service) handleCorrectMemory(w http.ResponseWriter, r *http.Request) {
	var d domain.MemoryDraft
	if err := readJSON(w, r, &d); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "invalid", "message": err.Error()})
		return
	}
	ver, found, err := s.CorrectMemory(r.Context(), r.PathValue("id"), d)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "invalid", "message": err.Error()})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "id": r.PathValue("id"), "version": ver})
}

func (s *Service) handleDeleteMemory(w http.ResponseWriter, r *http.Request) {
	found, err := s.SetMemoryState(r.Context(), r.PathValue("id"), domain.StateDeleted)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"status": "error", "message": err.Error()})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Service) handleMemoryAction(w http.ResponseWriter, r *http.Request) {
	id, action := r.PathValue("id"), r.PathValue("action")
	var found bool
	var err error
	switch action {
	case "enable", "grant":
		found, err = s.SetMemoryEnabled(r.Context(), id, true)
	case "disable", "revoke":
		found, err = s.SetMemoryEnabled(r.Context(), id, false)
	case "dispute":
		found, err = s.SetMemoryState(r.Context(), id, domain.StateDisputed)
	case "invalidate":
		found, err = s.SetMemoryState(r.Context(), id, domain.StateInvalid)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "invalid"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"status": "error", "message": err.Error()})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Service) handleImportMemories(w http.ResponseWriter, r *http.Request) {
	var in domain.ImportInput
	if err := readJSON(w, r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, domain.ImportResult{Status: "invalid", Message: err.Error()})
		return
	}
	res, err := s.ImportDocument(r.Context(), in)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, domain.ImportResult{Status: "error", Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Service) handleGetModels(w http.ResponseWriter, r *http.Request) {
	view := s.ModelConfigView(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "model_config": view})
}

func (s *Service) handleUpdateModel(w http.ResponseWriter, r *http.Request) {
	var in domain.ModelEndpointInput
	if err := readJSON(w, r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "invalid", "message": err.Error()})
		return
	}
	view, err := s.UpdateModelEndpoint(r.Context(), r.PathValue("role"), in)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "invalid", "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "model_config": view})
}

func (s *Service) handleTestModel(w http.ResponseWriter, r *http.Request) {
	ms, err := s.TestModelEndpoint(r.Context(), r.PathValue("role"))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "message": err.Error(), "model_ms": ms})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "message": "连接测试成功", "model_ms": ms})
}

func (s *Service) handleUpdateWeb(w http.ResponseWriter, r *http.Request) {
	var in domain.WebConfig
	if err := readJSON(w, r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "invalid", "message": err.Error()})
		return
	}
	saved, err := s.UpdateWebConfig(r.Context(), in)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "invalid", "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "web": saved})
}

func (s *Service) handleGetStrategy(w http.ResponseWriter, r *http.Request) {
	ver := 0
	if raw := r.URL.Query().Get("version"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"status": "invalid"})
			return
		}
		ver = parsed
	}
	pol, err := s.store.Policy().LoadVersion(ver)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "strategy": pol.Label(), "policy": pol})
}

func (s *Service) handleStrategyHistory(w http.ResponseWriter, _ *http.Request) {
	hist, err := s.PolicyHistory()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"status": "error", "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "policy_history": hist})
}

func (s *Service) handleUpdateSetting(w http.ResponseWriter, r *http.Request) {
	var item domain.Setting
	if err := readJSON(w, r, &item); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "invalid", "message": err.Error()})
		return
	}
	pol, err := s.UpdateSetting(r.Context(), r.PathValue("key"), item)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "invalid", "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "strategy": pol.Label(), "policy": pol})
}

func (s *Service) handleUpdatePrompt(w http.ResponseWriter, r *http.Request) {
	var item domain.PromptSetting
	if err := readJSON(w, r, &item); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "invalid", "message": err.Error()})
		return
	}
	pol, err := s.UpdatePrompt(r.Context(), r.PathValue("key"), item)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "invalid", "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "strategy": pol.Label(), "policy": pol})
}

func (s *Service) handleRollback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Version int `json:"version"`
	}
	if err := readJSON(w, r, &body); err != nil || body.Version <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "invalid"})
		return
	}
	pol, err := s.RollbackPolicy(r.Context(), body.Version)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "invalid", "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "strategy": pol.Label(), "policy": pol})
}

func (s *Service) handleListProposals(w http.ResponseWriter, r *http.Request) {
	props, err := s.ListProposals(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"status": "error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "proposals": props})
}

func (s *Service) handleResolveProposal(w http.ResponseWriter, r *http.Request) {
	decision := r.PathValue("decision")
	if decision != "approve" && decision != "reject" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "invalid"})
		return
	}
	status, pol, err := s.ResolveProposal(r.Context(), r.PathValue("id"), decision == "approve")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"status": "error", "message": err.Error()})
		return
	}
	respStatus := "ok"
	if status != "approved" && status != "rejected" {
		respStatus = status
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": respStatus, "resolution": status, "strategy": pol.Label(), "policy": pol,
	})
}

func (s *Service) handleListObservations(w http.ResponseWriter, r *http.Request) {
	obs, err := s.ListObservations(r.Context(), r.URL.Query().Get("id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"status": "error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "observations": obs})
}

func (s *Service) handleRecordObservation(w http.ResponseWriter, r *http.Request) {
	var o domain.Observation
	if err := readJSON(w, r, &o); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "invalid", "message": err.Error()})
		return
	}
	found, err := s.RecordObservation(r.Context(), o)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "invalid", "message": err.Error()})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Service) handleConfirmObservation(w http.ResponseWriter, r *http.Request) {
	found, err := s.ConfirmObservation(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"status": "error"})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Service) handleAudit(w http.ResponseWriter, r *http.Request) {
	var before int64
	if raw := r.URL.Query().Get("before"); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
			before = parsed
		}
	}
	entries, err := s.AuditPage(r.Context(), before)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"status": "error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "audit": entries})
}
