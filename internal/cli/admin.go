package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/coxlong/mnevo/internal/domain"
)

// AdminCommand describes a parsed CLI administrative command mapped to a REST request.
type AdminCommand struct {
	Method  string
	Path    string
	Body    any
	Timeout time.Duration
}

// ParseAdminCommand translates CLI administrative subcommands into REST calls against /v1/*.
func ParseAdminCommand(args []string) (AdminCommand, error) {
	if len(args) == 0 {
		return AdminCommand{}, errors.New("missing command")
	}
	op := args[0]
	action := ""
	if len(args) > 1 {
		action = args[1]
	}

	switch op {
	case "audit":
		if len(args) == 1 {
			return AdminCommand{Method: http.MethodGet, Path: "/v1/audit", Timeout: domain.IPCBudget}, nil
		}
		if len(args) == 2 {
			if id, err := strconv.ParseInt(args[1], 10, 64); err == nil && id > 0 {
				return AdminCommand{
					Method:  http.MethodGet,
					Path:    "/v1/audit?before=" + strconv.FormatInt(id, 10),
					Timeout: domain.IPCBudget,
				}, nil
			}
		}

	case "import":
		if len(args) == 2 {
			data, err := os.ReadFile(args[1])
			if err != nil {
				return AdminCommand{}, err
			}
			return AdminCommand{
				Method:  http.MethodPost,
				Path:    "/v1/memories/import",
				Body:    domain.ImportInput{Location: filepath.Clean(args[1]), Content: string(data)},
				Timeout: domain.LongOpBudget,
			}, nil
		}

	case "memory":
		switch action {
		case "list":
			if len(args) == 2 {
				return AdminCommand{Method: http.MethodGet, Path: "/v1/memories", Timeout: domain.IPCBudget}, nil
			}
			if len(args) == 3 {
				return AdminCommand{
					Method:  http.MethodGet,
					Path:    "/v1/memories?q=" + url.QueryEscape(args[2]),
					Timeout: domain.IPCBudget,
				}, nil
			}
		case "show":
			if len(args) == 3 {
				return AdminCommand{
					Method:  http.MethodGet,
					Path:    "/v1/memories/" + url.PathEscape(args[2]),
					Timeout: domain.IPCBudget,
				}, nil
			}
		case "delete":
			if len(args) == 3 {
				return AdminCommand{
					Method:  http.MethodDelete,
					Path:    "/v1/memories/" + url.PathEscape(args[2]),
					Timeout: domain.IPCBudget,
				}, nil
			}
		case "enable", "grant", "disable", "revoke", "dispute", "invalidate":
			if len(args) == 3 {
				return AdminCommand{
					Method:  http.MethodPost,
					Path:    "/v1/memories/" + url.PathEscape(args[2]) + "/" + action,
					Timeout: domain.IPCBudget,
				}, nil
			}
		case "add":
			if len(args) == 3 {
				var draft domain.MemoryDraft
				if err := domain.DecodeDocument(strings.NewReader(args[2]), &draft); err != nil {
					return AdminCommand{}, err
				}
				return AdminCommand{
					Method:  http.MethodPost,
					Path:    "/v1/memories",
					Body:    draft,
					Timeout: domain.IPCBudget,
				}, nil
			}
		case "correct":
			if len(args) == 4 {
				var draft domain.MemoryDraft
				if err := domain.DecodeDocument(strings.NewReader(args[3]), &draft); err != nil {
					return AdminCommand{}, err
				}
				return AdminCommand{
					Method:  http.MethodPut,
					Path:    "/v1/memories/" + url.PathEscape(args[2]),
					Body:    draft,
					Timeout: domain.IPCBudget,
				}, nil
			}
		case "import":
			if len(args) == 3 {
				data, err := os.ReadFile(args[2])
				if err != nil {
					return AdminCommand{}, err
				}
				return AdminCommand{
					Method:  http.MethodPost,
					Path:    "/v1/memories/import",
					Body:    domain.ImportInput{Location: filepath.Clean(args[2]), Content: string(data)},
					Timeout: domain.LongOpBudget,
				}, nil
			}
		}

	case "web":
		switch action {
		case "show":
			if len(args) == 2 {
				return AdminCommand{Method: http.MethodGet, Path: "/v1/models", Timeout: domain.IPCBudget}, nil
			}
		case "set":
			if len(args) == 3 {
				return AdminCommand{
					Method:  http.MethodPut,
					Path:    "/v1/web",
					Body:    domain.WebConfig{PublicURL: args[2]},
					Timeout: domain.IPCBudget,
				}, nil
			}
		case "clear":
			if len(args) == 2 {
				return AdminCommand{
					Method:  http.MethodPut,
					Path:    "/v1/web",
					Body:    domain.WebConfig{PublicURL: ""},
					Timeout: domain.IPCBudget,
				}, nil
			}
		}

	case "model":
		switch action {
		case "show":
			if len(args) == 2 {
				return AdminCommand{Method: http.MethodGet, Path: "/v1/models", Timeout: domain.IPCBudget}, nil
			}
		case "test":
			if len(args) == 3 {
				return AdminCommand{
					Method:  http.MethodPost,
					Path:    "/v1/models/" + url.PathEscape(args[2]) + "/test",
					Timeout: domain.LongOpBudget,
				}, nil
			}
		case "set":
			if len(args) == 4 {
				var ep domain.ModelEndpointInput
				if err := domain.DecodeDocument(strings.NewReader(args[3]), &ep); err != nil {
					return AdminCommand{}, err
				}
				return AdminCommand{
					Method:  http.MethodPut,
					Path:    "/v1/models/" + url.PathEscape(args[2]),
					Body:    ep,
					Timeout: domain.IPCBudget,
				}, nil
			}
		}

	case "strategy":
		switch action {
		case "show":
			if len(args) == 2 {
				return AdminCommand{Method: http.MethodGet, Path: "/v1/strategy", Timeout: domain.IPCBudget}, nil
			}
			if len(args) == 3 {
				if ver, err := strconv.Atoi(args[2]); err == nil && ver > 0 {
					return AdminCommand{
						Method:  http.MethodGet,
						Path:    "/v1/strategy?version=" + strconv.Itoa(ver),
						Timeout: domain.IPCBudget,
					}, nil
				}
			}
		case "history":
			if len(args) == 2 {
				return AdminCommand{Method: http.MethodGet, Path: "/v1/strategy/history", Timeout: domain.IPCBudget}, nil
			}
		case "proposals":
			if len(args) == 2 {
				return AdminCommand{Method: http.MethodGet, Path: "/v1/proposals", Timeout: domain.IPCBudget}, nil
			}
		case "evidence":
			if len(args) == 2 {
				return AdminCommand{Method: http.MethodGet, Path: "/v1/observations", Timeout: domain.IPCBudget}, nil
			}
			if len(args) == 3 {
				return AdminCommand{
					Method:  http.MethodGet,
					Path:    "/v1/observations?id=" + url.QueryEscape(args[2]),
					Timeout: domain.IPCBudget,
				}, nil
			}
		case "set":
			if len(args) == 4 {
				var s domain.Setting
				if err := domain.DecodeDocument(strings.NewReader(args[3]), &s); err != nil {
					return AdminCommand{}, err
				}
				return AdminCommand{
					Method:  http.MethodPut,
					Path:    "/v1/strategy/settings/" + url.PathEscape(args[2]),
					Body:    s,
					Timeout: domain.IPCBudget,
				}, nil
			}
		case "set-prompt":
			if len(args) == 4 {
				var p domain.PromptSetting
				if err := domain.DecodeDocument(strings.NewReader(args[3]), &p); err != nil {
					return AdminCommand{}, err
				}
				return AdminCommand{
					Method:  http.MethodPut,
					Path:    "/v1/strategy/prompts/" + url.PathEscape(args[2]),
					Body:    p,
					Timeout: domain.IPCBudget,
				}, nil
			}
		case "rollback":
			if len(args) == 3 {
				if ver, err := strconv.Atoi(args[2]); err == nil && ver > 0 {
					return AdminCommand{
						Method:  http.MethodPost,
						Path:    "/v1/strategy/rollback",
						Body:    map[string]int{"version": ver},
						Timeout: domain.IPCBudget,
					}, nil
				}
			}
		case "approve", "reject":
			if len(args) == 3 {
				return AdminCommand{
					Method:  http.MethodPost,
					Path:    "/v1/proposals/" + url.PathEscape(args[2]) + "/" + action,
					Timeout: domain.IPCBudget,
				}, nil
			}
		case "confirm-evidence":
			if len(args) == 3 {
				return AdminCommand{
					Method:  http.MethodPost,
					Path:    "/v1/observations/" + url.PathEscape(args[2]) + "/confirm",
					Timeout: domain.IPCBudget,
				}, nil
			}
		case "observe":
			if len(args) == 5 {
				return AdminCommand{
					Method:  http.MethodPost,
					Path:    "/v1/observations",
					Body:    domain.Observation{Kind: args[2], Task: args[3], Evidence: args[4]},
					Timeout: domain.IPCBudget,
				}, nil
			}
		}
	}
	return AdminCommand{}, fmt.Errorf("invalid %s command", op)
}
