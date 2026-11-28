package recall

import (
	"context"
	"encoding/json"
	"iter"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/coxlong/mnevo/internal/domain"
	"github.com/coxlong/mnevo/internal/model"
)

// Candidate is the snapshot read from storage before model evaluation.
type Candidate struct {
	ID           string
	Kind         string
	Summary      string
	Details      string
	Conditions   string
	TagsJSON     string
	Version      int
	Strength     float64
	ReinforcedAt int64
	Vector       []byte // Valid until the iterator advances.
	Tags         []string
}

type Evaluation struct {
	Candidate
	Similarity, Score, Probability float64
	Outcome                        string
	Latency                        int64
}

type DeliveryPlan struct {
	Evaluations  []Evaluation
	PromptDigest string
	EmbeddingMS  int64
	Characters   int
	ReferenceTTL time.Duration
	WantsDetail  bool
}

// Repository contains the persistence operations used by a recall.
type Repository interface {
	RecallCandidates(ctx context.Context, modelID string, nowMS int64) iter.Seq2[Candidate, error]
	RecordRecallOutcome(ctx context.Context, task domain.Task, result domain.RecallResult, promptDigest string) error
	CommitRecall(ctx context.Context, task domain.Task, result domain.RecallResult, plan DeliveryPlan) (domain.RecallResult, error)
}

type rankedCandidate struct {
	Candidate
	sim, score float64
}

// Run ranks eligible memories and applies the configured model gate under one policy snapshot.
func Run(ctx context.Context, repo Repository, providers model.Providers, t domain.Task, input model.RecallInput, pol domain.Policy, now func() time.Time) (domain.RecallResult, error) {
	prompt := input.Prompt
	promptDigest := domain.Digest(prompt)
	result := domain.RecallResult{
		Status:   "no_match",
		Strategy: pol.Label(),
	}
	if t.ID == "" || strings.TrimSpace(prompt) == "" {
		return result, nil
	}

	recordOutcome := func() (domain.RecallResult, error) {
		return result, repo.RecordRecallOutcome(ctx, t, result, promptDigest)
	}
	if !providers.RecallReady() {
		result.ModelOutcome = "models_not_configured"
		return recordOutcome()
	}
	query, skipped := prepareQuery(ctx, providers, input, pol.Value("recall.jev_threshold"))
	if skipped {
		result.ModelOutcome = "query_jev_skip"
		return recordOutcome()
	}

	// Step 1: Embed query prompt within recall.embedding_ms (outside any write tx).
	embedStarted := time.Now()
	embedCtx, cancelEmbed := context.WithTimeout(ctx, domain.Milliseconds(pol.Value("recall.embedding_ms")))
	vecs, embedErr := providers.Embedder.Embed(embedCtx, []string{query})
	embedMS := max(1, time.Since(embedStarted).Milliseconds())
	embedTimedOut := embedCtx.Err() == context.DeadlineExceeded
	cancelEmbed()

	if embedTimedOut {
		result.Status = "timeout"
		result.ModelOutcome = "embedding_timeout"
		result.ModelMS = embedMS
		return recordOutcome()
	}
	if embedErr != nil || len(vecs) != 1 || len(vecs[0]) == 0 {
		result.ModelOutcome = "embedding_error"
		result.ModelMS = embedMS
		return recordOutcome()
	}
	queryVec := vecs[0]

	// Step 2: Vector cosine similarity × exponential time decay over active embedded memories.
	nowMS := now().UnixMilli()
	candidates, err := rankCandidates(ctx, repo, pol, providers.Embedder.ModelID(), queryVec, nowMS)
	if err != nil {
		return result, err
	}
	if len(candidates) == 0 {
		result.ModelMS = embedMS
		return recordOutcome()
	}

	// Step 3: Jev (System One) situational relevance gate across Top-K vector candidates.
	jevBudget := domain.Milliseconds(pol.Value("recall.jev_ms"))
	if dl, ok := ctx.Deadline(); ok {
		if avail := time.Until(dl) - 30*time.Millisecond; avail < jevBudget {
			jevBudget = max(0, avail)
		}
	}
	jevDeadline := time.Now().Add(jevBudget)
	jevThreshold := pol.Value("recall.jev_threshold")
	jevInstructions := pol.Prompt("jev_recall")
	limit := int(pol.Value("recall.limit"))
	taskCue := domain.TruncateRunes(prompt, 600)

	evaluated := make([]Evaluation, 0, len(candidates))
	kept := 0
	for _, c := range candidates {
		if kept >= limit {
			break
		}
		rem := time.Until(jevDeadline)
		if rem <= 0 {
			evaluated = append(evaluated, Evaluation{Candidate: c.Candidate, Similarity: c.sim, Score: c.score, Outcome: "jev_timeout"})
			break
		}
		callCtx, cancel := context.WithTimeout(ctx, rem)
		started := time.Now()
		state := map[string]any{
			"task_prompt":  taskCue,
			"memory_type":  c.Kind,
			"summary":      c.Summary,
			"conditions":   c.Conditions,
			"tags":         c.Tags,
			"vector_score": math.Round(c.sim*1000) / 1000,
		}
		if input.PreviousAnswer != "" {
			state["retrieval_query"] = query
		}
		prob, callErr := providers.Gate.Decide(callCtx, jevInstructions, state)
		latency := max(1, time.Since(started).Milliseconds())
		timedOut := callCtx.Err() == context.DeadlineExceeded
		cancel()

		evaluation := Evaluation{Candidate: c.Candidate, Similarity: c.sim, Score: c.score, Latency: latency}
		switch {
		case timedOut:
			evaluation.Outcome = "jev_timeout"
		case callErr != nil:
			evaluation.Outcome = "jev_error"
		case prob <= jevThreshold:
			evaluation.Outcome = "jev_skip"
			evaluation.Probability = prob
		default:
			evaluation.Outcome = "jev_keep"
			evaluation.Probability = prob
			kept++
		}
		evaluated = append(evaluated, evaluation)
	}

	wantsDetail := strings.Contains(prompt, "详情") || strings.Contains(prompt, "证据") || strings.Contains(strings.ToLower(prompt), "detail")
	return repo.CommitRecall(ctx, t, result, DeliveryPlan{
		Evaluations:  evaluated,
		PromptDigest: promptDigest,
		EmbeddingMS:  embedMS,
		Characters:   int(pol.Value("recall.characters")),
		ReferenceTTL: time.Duration(pol.Value("recall.reference_minutes") * float64(time.Minute)),
		WantsDetail:  wantsDetail,
	})
}

// rankCandidates retains only the best K rows while scanning. K is small (at most 16),
// so keeping this slice sorted avoids buffering and sorting every eligible memory.
func rankCandidates(ctx context.Context, repo Repository, pol domain.Policy, modelID string, query []float32, nowMS int64) ([]rankedCandidate, error) {
	embedMin := pol.Value("recall.embedding_min")
	scoreMin := pol.Value("recall.minimum")
	k := int(pol.Value("recall.jev_candidates"))
	best := make([]rankedCandidate, 0, k+1)
	vector := make([]float32, len(query))
	for c, err := range repo.RecallCandidates(ctx, modelID, nowMS) {
		if err != nil {
			return nil, err
		}
		if !model.DecodeVectorInto(vector, c.Vector) {
			continue
		}
		sim := model.CosineSimilarity(query, vector)
		if sim < embedMin {
			continue
		}
		age := math.Max(0, float64(nowMS-c.ReinforcedAt))
		halfLife := pol.Value("decay."+c.Kind+"_days") * float64((24 * time.Hour).Milliseconds())
		score := sim * (c.Strength * math.Exp2(-age/halfLife))
		if score < scoreMin {
			continue
		}
		c.Vector = nil
		ranked := rankedCandidate{Candidate: c, sim: sim, score: score}
		index, _ := slices.BinarySearchFunc(best, ranked, compareRank)
		if index < k {
			best = slices.Insert(best, index, ranked)
			if len(best) > k {
				best[k] = rankedCandidate{}
				best = best[:k]
			}
		}
	}
	for i := range best {
		_ = json.Unmarshal([]byte(best[i].TagsJSON), &best[i].Tags)
	}
	return best, nil
}

func compareRank(a, b rankedCandidate) int {
	switch {
	case a.score > b.score:
		return -1
	case a.score < b.score:
		return 1
	default:
		return strings.Compare(a.ID, b.ID)
	}
}
