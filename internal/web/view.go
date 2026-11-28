package web

import (
	"fmt"
	"net/url"
	"time"
)

func formatTime(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04:05")
}

func kindLabel(kind string) string {
	labels := map[string]string{
		"fact": "事实", "guideline": "准则", "procedure": "方法",
		"preference": "偏好", "episode": "经历",
	}
	if label := labels[kind]; label != "" {
		return label
	}
	return kind
}

func viewTitle(view string) string {
	switch view {
	case "models":
		return "模型配置"
	case "strategy":
		return "策略与提案"
	case "evidence":
		return "诊断证据"
	case "audit":
		return "审计记录"
	default:
		return "记忆库"
	}
}

func viewEyebrow(view string) string {
	switch view {
	case "models":
		return "Model connections"
	case "strategy":
		return "Policy & evolution"
	case "evidence":
		return "Trusted evidence"
	case "audit":
		return "System activity"
	default:
		return "Long-term memory"
	}
}

func memoryListTitle(p pageData) string {
	if p.ID != "" {
		return "全部版本"
	}
	if p.Query != "" {
		return "搜索结果"
	}
	return "最近记忆"
}

func memoryListURL(p pageData) string {
	values := url.Values{"view": {"memory"}}
	if p.Query != "" {
		values.Set("q", p.Query)
	}
	return "?" + values.Encode()
}

func memorySelectURL(p pageData, id string) string {
	values := url.Values{"view": {"memory"}, "selected": {id}}
	if p.Query != "" {
		values.Set("q", p.Query)
	}
	return "?" + values.Encode()
}

func memoryDetailURL(p pageData, id string) string {
	values := url.Values{"id": {id}}
	if p.Query != "" {
		values.Set("q", p.Query)
	}
	return "?" + values.Encode()
}

func auditNextURL(before int64) string {
	return fmt.Sprintf("?view=audit&before=%d", before)
}
