package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/coxlong/mnevo/internal/domain"
)

var kindUsageLabels = map[string]string{
	domain.KindFact:       "事实核对",
	domain.KindEpisode:    "历史参考，使用前重新核验",
	domain.KindProcedure:  "操作提示",
	domain.KindPreference: "用户偏好",
	domain.KindGuideline:  "检查提醒",
}

// HookClient sends hook lifecycle events to the local Mnevo daemon.
type HookClient interface {
	PostHook(ctx context.Context, req domain.HookRequest) (domain.RecallResult, error)
}

// RunHook processes a single Claude Code hook event from stdin and writes any
// injected memory context to stdout. Errors degrade gracefully so Claude Code never blocks.
func RunHook(ctx context.Context, client HookClient, in io.Reader, out io.Writer) error {
	var event struct {
		Session        string `json:"session_id"`
		Name           string `json:"hook_event_name"`
		Prompt         string `json:"prompt"`
		TranscriptPath string `json:"transcript_path"`
	}
	if err := json.NewDecoder(io.LimitReader(in, domain.MaxMessage/2)).Decode(&event); err != nil {
		slog.Warn("hook ignored malformed payload", "error", err)
		return nil
	}
	switch event.Name {
	case "UserPromptSubmit", "Stop", "StopFailure", "SessionEnd":
	default:
		return nil
	}

	result, err := client.PostHook(ctx, domain.HookRequest{
		SessionID:      event.Session,
		Event:          event.Name,
		Prompt:         event.Prompt,
		TranscriptPath: event.TranscriptPath,
	})
	if err != nil {
		slog.Warn("hook skipped", "event", event.Name, "error", err)
		return nil
	}

	var additional strings.Builder
	switch result.Status {
	case "accepted":
		fmt.Fprintf(&additional, "Mnevo 已收到用户确认的记忆，保存回执 %s。", result.Receipt)
	case "invalid":
		additional.WriteString("Mnevo 未能解析记忆指令。可直接输入「记住：<自然语言>」或规范 JSON。")
	case "delivered":
		if len(result.Recalls) == 0 {
			return nil
		}
		for _, entry := range result.Recalls {
			fmt.Fprintf(&additional, "Mnevo %s（%s）\n%s\n适用条件 %s\n记忆引用 %s\n",
				kindUsageLabels[entry.Kind], entry.Layer, entry.Summary, entry.Conditions, entry.Reference)
		}
		additional.WriteString("可通过 mnevo 的 get_memory 回查细节，发现变化可通过 submit_memory_feedback 提交反馈。")
	default:
		return nil
	}

	return json.NewEncoder(out).Encode(map[string]any{
		"hookSpecificOutput": map[string]string{
			"hookEventName":     event.Name,
			"additionalContext": additional.String(),
		},
	})
}
