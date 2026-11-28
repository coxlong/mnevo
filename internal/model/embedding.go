package model

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"net/http"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// OpenAIEmbedder calls an OpenAI-compatible /v1/embeddings endpoint (OpenRouter, OpenAI, Ollama, vLLM).
type OpenAIEmbedder struct {
	client openai.EmbeddingService
	model  string
}

// NewOpenAIEmbedder creates an OpenAIEmbedder with retries disabled so latency budgets remain predictable.
func NewOpenAIEmbedder(httpClient *http.Client, baseURL, modelName, apiKey string) *OpenAIEmbedder {
	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}
	client := openai.NewEmbeddingService(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(baseURL),
		option.WithHTTPClient(httpClient),
		option.WithMaxRetries(0),
	)
	return &OpenAIEmbedder{client: client, model: modelName}
}

// ModelID returns the identifier of the active embedding model (used to version stored vectors).
func (e *OpenAIEmbedder) ModelID() string { return e.model }

// Embed returns L2-normalized float32 embeddings corresponding to texts in order.
func (e *OpenAIEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	resp, err := e.client.New(ctx, openai.EmbeddingNewParams{
		Model: e.model,
		Input: openai.EmbeddingNewParamsInputUnion{
			OfArrayOfStrings: texts,
		},
		EncodingFormat: openai.EmbeddingNewParamsEncodingFormatFloat,
	})
	if err != nil {
		return nil, err
	}
	if len(resp.Data) != len(texts) {
		return nil, errors.New("embedding count mismatch")
	}
	vectors := make([][]float32, len(texts))
	for _, item := range resp.Data {
		idx := int(item.Index)
		if idx < 0 || idx >= len(texts) || len(item.Embedding) == 0 {
			return nil, errors.New("invalid embedding payload")
		}
		vec := make([]float32, len(item.Embedding))
		for i, v := range item.Embedding {
			vec[i] = float32(v)
		}
		vectors[idx] = NormalizeVector(vec)
	}
	for _, v := range vectors {
		if len(v) == 0 {
			return nil, errors.New("missing embedding vector")
		}
	}
	return vectors, nil
}

// NormalizeVector returns an L2-normalized copy of v (or v unchanged if zero-norm).
func NormalizeVector(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	norm := math.Sqrt(sum)
	if norm == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) {
		return v
	}
	out := make([]float32, len(v))
	inv := float32(1.0 / norm)
	for i, x := range v {
		out[i] = x * inv
	}
	return out
}

// EncodeVector serializes a float32 slice as compact little-endian bytes for SQLite BLOB storage.
func EncodeVector(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, x := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(x))
	}
	return buf
}

// DecodeVector deserializes a little-endian float32 BLOB back into a slice.
func DecodeVector(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	out := make([]float32, len(b)/4)
	DecodeVectorInto(out, b)
	return out
}

// DecodeVectorInto fills dst from a BLOB of the same number of dimensions.
func DecodeVectorInto(dst []float32, b []byte) bool {
	if len(dst)*4 != len(b) {
		return false
	}
	for i := range dst {
		dst[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return true
}

// CosineSimilarity computes cosine similarity between a and b (returns 0 on length mismatch or zero norm).
func CosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		fa, fb := float64(a[i]), float64(b[i])
		dot += fa * fb
		normA += fa * fa
		normB += fb * fb
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 || math.IsNaN(denom) || math.IsInf(denom, 0) {
		return 0
	}
	sim := dot / denom
	if sim > 1 {
		return 1
	}
	if sim < -1 {
		return -1
	}
	return sim
}
