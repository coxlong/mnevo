package model

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"resty.dev/v3"
)

// SystemOne implements DecisionGate against a System One / Jev HTTP endpoint via Resty v3.
type SystemOne struct {
	client   *resty.Client
	endpoint string
	model    string
	key      string
}

// NewSystemOne constructs a SystemOne client with retry disabled so latency budgets stay bounded.
func NewSystemOne(httpClient *http.Client, endpoint, modelName, apiKey string) *SystemOne {
	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}
	client := resty.NewWithClient(httpClient).SetRetryCount(0).SetResponseBodyLimit(64 << 10)
	return &SystemOne{
		client:   client,
		endpoint: endpoint,
		model:    modelName,
		key:      apiKey,
	}
}

// Decide evaluates a single binary "noul" probability question against state.
func (j *SystemOne) Decide(ctx context.Context, instructions string, state any) (float64, error) {
	var result struct {
		Answers map[string]struct {
			Type        string   `json:"type"`
			Probability *float64 `json:"noul"`
		} `json:"answers"`
	}
	req := j.client.R().
		SetContext(ctx).
		SetHeader("Content-Type", "application/json").
		SetResult(&result).
		SetBody(map[string]any{
			"model": j.model,
			"state": state,
			"questions": map[string]any{
				"decision": map[string]string{
					"type":         "noul",
					"instructions": instructions,
				},
			},
		})
	if j.key != "" {
		req.SetAuthToken(j.key)
	}
	resp, err := req.Post(j.endpoint)
	if err != nil {
		return 0, err
	}
	if !resp.IsStatusSuccess() {
		return 0, fmt.Errorf("System One returned HTTP %d", resp.StatusCode())
	}
	answer := result.Answers["decision"]
	if answer.Type != "noul" || answer.Probability == nil || *answer.Probability < 0 || *answer.Probability > 1 {
		return 0, errors.New("invalid System One decision answer")
	}
	return *answer.Probability, nil
}
