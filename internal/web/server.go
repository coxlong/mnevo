package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/coxlong/mnevo/internal/domain"
)

// Backend defines the clean typed operations needed by the local Web management UI.
type Backend interface {
	ListMemories(ctx context.Context, filter, id string) ([]domain.MemoryRecord, error)
	CreateMemory(ctx context.Context, d domain.MemoryDraft) (string, int, error)
	CorrectMemory(ctx context.Context, id string, d domain.MemoryDraft) (int, bool, error)
	SetMemoryEnabled(ctx context.Context, id string, enabled bool) (bool, error)
	SetMemoryState(ctx context.Context, id, state string) (bool, error)
	ImportDocument(ctx context.Context, in domain.ImportInput) (domain.ImportResult, error)

	PublicURL() string
	UpdateWebConfig(ctx context.Context, cfg domain.WebConfig) (domain.WebConfig, error)
	ModelConfigView(ctx context.Context) domain.ModelConfigView
	UpdateModelEndpoint(ctx context.Context, role string, in domain.ModelEndpointInput) (domain.ModelConfigView, error)
	TestModelEndpoint(ctx context.Context, role string) (int64, error)

	ActivePolicy() domain.Policy
	PolicyHistory() ([]domain.PolicyRevision, error)
	UpdateSetting(ctx context.Context, key string, item domain.Setting) (domain.Policy, error)
	UpdatePrompt(ctx context.Context, key string, item domain.PromptSetting) (domain.Policy, error)
	RollbackPolicy(ctx context.Context, version int) (domain.Policy, error)
	ListProposals(ctx context.Context) ([]domain.Proposal, error)
	ResolveProposal(ctx context.Context, id string, approve bool) (string, domain.Policy, error)
	ListObservations(ctx context.Context, id string) ([]domain.ObservationRecord, error)
	RecordObservation(ctx context.Context, o domain.Observation) (bool, error)
	ConfirmObservation(ctx context.Context, id string) (bool, error)

	AuditPage(ctx context.Context, before int64) ([]domain.AuditEntry, error)
}

type settingRow struct {
	Key  string
	Item domain.Setting
}

type promptRow struct {
	Key  string
	Item domain.PromptSetting
}

type viewResult struct {
	Memories     []domain.MemoryRecord
	Policy       *domain.Policy
	Proposals    []domain.Proposal
	Observations []domain.ObservationRecord
	Audit        []domain.AuditEntry
}

type pageData struct {
	View         string
	Query        string
	ID           string
	Selection    string
	Preview      *domain.MemoryRecord
	Before       int64
	NextBefore   int64
	CSRF         string
	Base         string
	Action       string
	Message      string
	SettingsJSON string
	SettingRows  []settingRow
	PromptRows   []promptRow
	ModelConfig  *domain.ModelConfigView
	Result       viewResult
	History      []domain.PolicyRevision
}

// NewHandler constructs the session-authenticated, CSRF-protected management UI.
func NewHandler(backend Backend, adminKey, origin string, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	sessions := newSessionStore(adminKey, origin)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")

		pubHost, pubOrigin, pubPath := parsePublicURL(backend.PublicURL())
		if !allowedHost(r.Host, pubHost) {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/favicon.ico" || (pubPath != "" && r.URL.Path == pubPath+"/favicon.ico") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		route, ok := webRoute(r.URL.Path, pubPath)
		if !ok {
			http.NotFound(w, r)
			return
		}
		base := requestBase(r.URL.Path, pubPath)
		if strings.HasPrefix(route, "assets/") {
			serveAsset(w, r, route)
			return
		}
		if route == "login" {
			handleLogin(w, r, sessions, base, pubOrigin, pubPath)
			return
		}

		sessionID, session, authenticated := sessions.get(r)
		if !authenticated {
			loginURL := base + "login?next=" + url.QueryEscape(r.URL.RequestURI())
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", loginURL)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
			} else {
				http.Redirect(w, r, loginURL, http.StatusSeeOther)
			}
			return
		}
		if route == "logout" {
			handleLogout(w, r, sessions, sessionID, session, base, pubOrigin)
			return
		}
		if route != "root" {
			http.NotFound(w, r)
			return
		}

		page := pageData{
			View: r.URL.Query().Get("view"), Query: r.URL.Query().Get("q"),
			ID: r.URL.Query().Get("id"), Selection: r.URL.Query().Get("selected"),
			CSRF: session.CSRF, Base: base, Action: r.URL.RequestURI(),
		}
		if b := r.URL.Query().Get("before"); b != "" {
			if parsed, err := strconv.ParseInt(b, 10, 64); err == nil && parsed > 0 {
				page.Before = parsed
			}
		}

		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, domain.MaxMessage)
			if !allowedOrigin(r, pubOrigin) || r.ParseForm() != nil ||
				subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(session.CSRF)) != 1 {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			msg, targetView, err := executeMutation(r.Context(), backend, r)
			if targetView != "" {
				page.View = targetView
			}
			if err != nil {
				page.Message = "操作失败：" + err.Error()
				logger.Warn("management mutation failed", "error", err)
			} else {
				page.Message = msg
			}

			// htmx requests render the updated fragment inline (with the message).
			if r.Header.Get("HX-Request") == "true" {
				sessions.setFlash(sessionID, page.Message)
				page.Message = sessions.popFlash(sessionID)
				if err := populateView(r.Context(), backend, &page); err != nil {
					logger.Error("management query failed", "view", page.View, "error", err)
					http.Error(w, "Unavailable", http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				if renderErr := partial(page).Render(r.Context(), w); renderErr != nil {
					logger.Error("management render failed", "error", renderErr)
				}
				return
			}

			// Regular form POST: PRG redirect so refresh never resubmits; the
			// result message survives via a one-shot flash on the session.
			sessions.setFlash(sessionID, page.Message)
			redirect := base + "?" + redirectQuery(page.View)
			http.Redirect(w, r, redirect, http.StatusSeeOther)
			return
		} else if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Display (and consume) any pending flash message from the previous POST.
		page.Message = sessions.popFlash(sessionID)

		if err := populateView(r.Context(), backend, &page); err != nil {
			logger.Error("management query failed", "view", page.View, "error", err)
			http.Error(w, "Unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		component := pageTemplate(page)
		if r.Header.Get("HX-Request") == "true" {
			component = partial(page)
		}
		if err := component.Render(r.Context(), w); err != nil {
			logger.Error("management render failed", "error", err)
		}
	})
}

func parsePublicURL(raw string) (host, origin, basePath string) {
	if raw == "" {
		return "", "", ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", "", ""
	}
	return u.Host, u.Scheme + "://" + u.Host, strings.TrimRight(u.Path, "/")
}

func isLoopbackHost(hostPort string) bool {
	if hostPort == "" {
		return false
	}
	host := hostPort
	if h, _, err := net.SplitHostPort(hostPort); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func allowedHost(reqHost, pubHost string) bool {
	if isLoopbackHost(reqHost) {
		return true
	}
	if pubHost == "" {
		return false
	}
	return strings.EqualFold(reqHost, pubHost)
}

func webRoute(reqPath, pubPath string) (string, bool) {
	if reqPath == "/" || (pubPath != "" && (reqPath == pubPath || reqPath == pubPath+"/")) {
		return "root", true
	}
	bases := []string{""}
	if pubPath != "" {
		bases = append(bases, pubPath)
	}
	for _, base := range bases {
		rel, ok := strings.CutPrefix(reqPath, base+"/")
		if !ok {
			continue
		}
		switch rel {
		case "login", "logout", "assets/app.css", "assets/htmx.min.js":
			return rel, true
		}
	}
	return "", false
}

func requestBase(reqPath, pubPath string) string {
	if pubPath != "" && (reqPath == pubPath || strings.HasPrefix(reqPath, pubPath+"/")) {
		return pubPath + "/"
	}
	return "/"
}

// redirectQuery builds the query string for the post-mutation GET redirect.
func redirectQuery(view string) string {
	if view == "" || view == "memory" {
		return "view=memory"
	}
	return "view=" + url.QueryEscape(view)
}

func allowedOrigin(r *http.Request, pubOrigin string) bool {
	orig := r.Header.Get("Origin")
	if orig == "" || r.Host == "" {
		return false
	}
	if orig == "http://"+r.Host || orig == "https://"+r.Host {
		return true
	}
	return pubOrigin != "" && strings.EqualFold(orig, pubOrigin)
}

func executeMutation(ctx context.Context, b Backend, r *http.Request) (string, string, error) {
	op := r.PostForm.Get("operation")
	action := r.PostForm.Get("action")
	target := strings.TrimSpace(r.PostForm.Get("target"))

	switch op {
	case "memory":
		switch action {
		case "add":
			var d domain.MemoryDraft
			if err := parseMemoryDraftForm(r, &d); err != nil {
				return "", "memory", err
			}
			id, _, err := b.CreateMemory(ctx, d)
			return fmt.Sprintf("ok（已新增 %s）", id), "memory", err
		case "correct":
			var d domain.MemoryDraft
			if err := parseMemoryDraftForm(r, &d); err != nil {
				return "", "memory", err
			}
			ver, found, err := b.CorrectMemory(ctx, target, d)
			if err != nil {
				return "", "memory", err
			}
			if !found {
				return "unavailable", "memory", nil
			}
			return fmt.Sprintf("ok（已发布 v%d）", ver), "memory", nil
		case "import":
			res, err := b.ImportDocument(ctx, domain.ImportInput{
				Location: strings.TrimSpace(r.PostForm.Get("location")),
				Content:  r.PostForm.Get("content"),
			})
			if err != nil {
				return "", "memory", err
			}
			if res.Message != "" {
				return fmt.Sprintf("%s（%s，%dms）", res.Status, res.Message, res.ModelMS), "memory", nil
			}
			return res.Status, "memory", nil
		case "enable", "grant", "disable", "revoke":
			found, err := b.SetMemoryEnabled(ctx, target, action == "enable" || action == "grant")
			if err != nil || !found {
				return "unavailable", "memory", err
			}
			return "ok", "memory", nil
		case "dispute", "invalidate", "delete":
			state := map[string]string{
				"dispute": domain.StateDisputed, "invalidate": domain.StateInvalid, "delete": domain.StateDeleted,
			}[action]
			found, err := b.SetMemoryState(ctx, target, state)
			if err != nil || !found {
				return "unavailable", "memory", err
			}
			return "ok", "memory", nil
		default:
			return "", "memory", errors.New("invalid memory action")
		}

	case "web":
		if action != "set" {
			return "", "models", errors.New("invalid web action")
		}
		_, err := b.UpdateWebConfig(ctx, domain.WebConfig{
			PublicURL: strings.TrimSpace(r.PostForm.Get("public_url")),
		})
		return "ok（Web 入口配置已保存并热生效）", "models", err

	case "model":
		switch action {
		case "set":
			_, err := b.UpdateModelEndpoint(ctx, target, domain.ModelEndpointInput{
				Enabled:  r.PostForm.Get("enabled") == "on",
				URL:      strings.TrimSpace(r.PostForm.Get("url")),
				Model:    strings.TrimSpace(r.PostForm.Get("model")),
				APIKey:   strings.TrimSpace(r.PostForm.Get("api_key")),
				ClearKey: r.PostForm.Get("clear_key") == "on",
			})
			return "ok（已保存并热生效）", "models", err
		case "test":
			ms, err := b.TestModelEndpoint(ctx, target)
			if err != nil {
				return fmt.Sprintf("error：%s", err.Error()), "models", nil
			}
			return fmt.Sprintf("ok（连接测试成功，%dms）", ms), "models", nil
		default:
			return "", "models", errors.New("invalid model action")
		}

	case "strategy":
		switch action {
		case "approve", "reject":
			status, _, err := b.ResolveProposal(ctx, target, action == "approve")
			return status, "strategy", err
		case "confirm-evidence":
			found, err := b.ConfirmObservation(ctx, target)
			if err != nil || !found {
				return "unavailable", "evidence", err
			}
			return "confirmed", "evidence", nil
		case "rollback":
			v, err := strconv.Atoi(target)
			if err != nil || v <= 0 {
				return "", "strategy", errors.New("invalid strategy version")
			}
			pol, err := b.RollbackPolicy(ctx, v)
			if err != nil {
				return "", "strategy", err
			}
			return fmt.Sprintf("ok（已发布 v%d）", pol.Version), "strategy", nil
		case "set":
			val, err := strconv.ParseFloat(strings.TrimSpace(r.PostForm.Get("value")), 64)
			if err != nil {
				return "", "strategy", err
			}
			minB, err := strconv.ParseFloat(strings.TrimSpace(r.PostForm.Get("min")), 64)
			if err != nil {
				return "", "strategy", err
			}
			maxB, err := strconv.ParseFloat(strings.TrimSpace(r.PostForm.Get("max")), 64)
			if err != nil {
				return "", "strategy", err
			}
			apply := r.PostForm.Get("apply")
			if apply == "" {
				apply = "approval"
			}
			pol, err := b.UpdateSetting(ctx, target, domain.Setting{
				Value: val, Learn: r.PostForm.Get("learn") == "on", Apply: apply, Bounds: [2]float64{minB, maxB},
			})
			if err != nil {
				return "", "strategy", err
			}
			return fmt.Sprintf("ok（已发布 v%d）", pol.Version), "strategy", nil
		case "set-prompt":
			pol, err := b.UpdatePrompt(ctx, target, domain.PromptSetting{
				Value: strings.TrimSpace(r.PostForm.Get("prompt_value")),
				Learn: r.PostForm.Get("learn") == "on",
				Apply: "approval",
			})
			if err != nil {
				return "", "strategy", err
			}
			return fmt.Sprintf("ok（已发布 v%d）", pol.Version), "strategy", nil
		case "observe":
			found, err := b.RecordObservation(ctx, domain.Observation{
				Kind:     strings.TrimSpace(r.PostForm.Get("kind")),
				Task:     strings.TrimSpace(r.PostForm.Get("task")),
				Evidence: strings.TrimSpace(r.PostForm.Get("evidence")),
			})
			if err != nil || !found {
				return "unavailable", "evidence", err
			}
			return "confirmed", "evidence", nil
		default:
			return "", "strategy", errors.New("invalid strategy action")
		}

	default:
		return "", "", errors.New("invalid operation")
	}
}

func parseMemoryDraftForm(r *http.Request, dst *domain.MemoryDraft) error {
	if raw := strings.TrimSpace(r.PostForm.Get("payload")); raw != "" {
		return domain.DecodeDocument(strings.NewReader(raw), dst)
	}
	var tags []string
	for _, t := range strings.Split(r.PostForm.Get("tags"), ",") {
		if s := strings.TrimSpace(t); s != "" {
			tags = append(tags, s)
		}
	}
	*dst = domain.MemoryDraft{
		Kind:       strings.TrimSpace(r.PostForm.Get("kind")),
		Summary:    strings.TrimSpace(r.PostForm.Get("summary")),
		Conditions: strings.TrimSpace(r.PostForm.Get("conditions")),
		Details:    strings.TrimSpace(r.PostForm.Get("details")),
		Tags:       tags,
	}
	return nil
}

func populateView(ctx context.Context, b Backend, page *pageData) error {
	mc := b.ModelConfigView(ctx)
	page.ModelConfig = &mc

	switch page.View {
	case "models":
		return nil

	case "strategy":
		pol := b.ActivePolicy()
		proposals, err := b.ListProposals(ctx)
		if err != nil {
			return err
		}
		history, err := b.PolicyHistory()
		if err != nil {
			return err
		}
		page.Result.Policy = &pol
		page.Result.Proposals = proposals
		page.History = history

		keys := make([]string, 0, len(pol.Settings))
		for k := range pol.Settings {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			page.SettingRows = append(page.SettingRows, settingRow{Key: k, Item: pol.Settings[k]})
		}
		pkeys := make([]string, 0, len(pol.Prompts))
		for k := range pol.Prompts {
			pkeys = append(pkeys, k)
		}
		slices.Sort(pkeys)
		for _, k := range pkeys {
			page.PromptRows = append(page.PromptRows, promptRow{Key: k, Item: pol.Prompts[k]})
		}
		data, _ := json.MarshalIndent(pol, "", "  ")
		page.SettingsJSON = string(data)
		return nil

	case "evidence":
		obs, err := b.ListObservations(ctx, page.ID)
		if err != nil {
			return err
		}
		page.Result.Observations = obs
		return nil

	case "audit":
		entries, err := b.AuditPage(ctx, page.Before)
		if err != nil {
			return err
		}
		page.Result.Audit = entries
		if n := len(entries); n > 0 {
			page.NextBefore = entries[n-1].ID
		}
		return nil

	default:
		page.View = "memory"
		filter, id := page.Query, ""
		if page.ID != "" {
			filter, id = "", page.ID
		}
		mems, err := b.ListMemories(ctx, filter, id)
		if err != nil {
			return err
		}
		page.Result.Memories = mems
		if page.ID == "" && len(mems) > 0 {
			page.Preview = &mems[0]
			for i := range mems {
				if mems[i].ID == page.Selection {
					page.Preview = &mems[i]
					break
				}
			}
			page.Selection = page.Preview.ID
		}
		return nil
	}
}
