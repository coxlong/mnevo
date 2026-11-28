package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/coxlong/mnevo/internal/domain"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// OpenAIChat handles structured generation via an OpenAI-compatible Chat Completions service.
type OpenAIChat struct {
	client openai.ChatCompletionService
	model  string
}

// NewOpenAIChat constructs a unified LLM client backed by openai-go/v3.
func NewOpenAIChat(httpClient *http.Client, baseURL, modelName, apiKey string) *OpenAIChat {
	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}
	client := openai.NewChatCompletionService(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(baseURL),
		option.WithHTTPClient(httpClient),
		option.WithMaxRetries(0),
	)
	return &OpenAIChat{client: client, model: modelName}
}

func (c *OpenAIChat) completeJSON(ctx context.Context, systemPrompt, userContent string, maxTokens int64, dst any) error {
	output, err := c.client.New(ctx, openai.ChatCompletionNewParams{
		Model:               c.model,
		MaxCompletionTokens: openai.Int(maxTokens),
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &openai.ResponseFormatJSONObjectParam{},
		},
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
			openai.UserMessage(userContent),
		},
	})
	if err != nil {
		return err
	}
	if len(output.Choices) != 1 || output.Choices[0].FinishReason != "stop" {
		return errors.New("incomplete LLM JSON response")
	}
	dec := json.NewDecoder(strings.NewReader(output.Choices[0].Message.Content))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.Decode(new(any)) != io.EOF {
		return errors.New("unexpected trailing JSON tokens")
	}
	return nil
}

// RewriteRecall generates a short semantic query and source keywords in strict JSON mode.
func (c *OpenAIChat) RewriteRecall(ctx context.Context, instructions string, input RecallInput) (RecallQuery, error) {
	data, err := json.Marshal(input)
	if err != nil {
		return RecallQuery{}, err
	}
	var query RecallQuery
	if err := c.completeJSON(ctx, instructions, string(data), 160, &query); err != nil {
		return RecallQuery{}, err
	}
	if strings.TrimSpace(query.Query) == "" {
		return RecallQuery{}, errors.New("missing recall query")
	}
	return query, nil
}

// Extract structures raw user text, documents, or transcripts into normalized MemoryDrafts.
func (c *OpenAIChat) Extract(ctx context.Context, prompt, text string) ([]domain.MemoryDraft, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, nil
	}
	trimmed = domain.TruncateRunes(trimmed, MaxExtractionRunes)
	var parsed struct {
		Memories []domain.MemoryDraft `json:"memories"`
	}
	if err := c.completeJSON(ctx, prompt, trimmed, 1024, &parsed); err != nil {
		return nil, err
	}
	valid := make([]domain.MemoryDraft, 0, len(parsed.Memories))
	for _, d := range parsed.Memories {
		if d.Normalize() {
			valid = append(valid, d)
		}
	}
	return valid, nil
}

// Verify evaluates feedback against the current memory and optionally produces a revised draft.
func (c *OpenAIChat) Verify(ctx context.Context, prompt string, input FeedbackVerifyInput) (FeedbackVerifyResult, error) {
	data, err := json.Marshal(input)
	if err != nil {
		return FeedbackVerifyResult{}, err
	}
	var res FeedbackVerifyResult
	if err := c.completeJSON(ctx, prompt, string(data), 512, &res); err != nil {
		return FeedbackVerifyResult{}, err
	}
	if res.ShouldRevise == nil {
		return FeedbackVerifyResult{}, errors.New("missing should_revise boolean")
	}
	return res, nil
}

// Diagnose analyzes verified diagnostic evidence and proposes a concrete strategy/prompt fix or advice.
func (c *OpenAIChat) Diagnose(ctx context.Context, prompt string, input MetaDiagnosisInput) (MetaDiagnosisResult, error) {
	data, err := json.Marshal(input)
	if err != nil {
		return MetaDiagnosisResult{}, err
	}
	var res MetaDiagnosisResult
	if err := c.completeJSON(ctx, prompt, string(data), 768, &res); err != nil {
		return MetaDiagnosisResult{}, err
	}
	switch res.TargetType {
	case "setting", "prompt", "advice":
	default:
		return MetaDiagnosisResult{}, errors.New("invalid meta diagnosis target_type")
	}
	if strings.TrimSpace(res.TargetKey) == "" || strings.TrimSpace(res.Diagnosis) == "" {
		return MetaDiagnosisResult{}, errors.New("incomplete meta diagnosis proposal")
	}
	return res, nil
}
