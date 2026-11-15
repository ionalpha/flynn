// Package hybrid ranks recall by the store's word match and an embedding's meaning
// match, fused by reciprocal rank. A fact asked for in different words than it was
// written in shares no term with the query, so a lexical index misses it; an
// identifier or error code is matched exactly by the index and blurred by an
// embedding. Fusing covers both.
//
// The host supplies the model as an [Embedder]. A Store without one delegates every
// recall unchanged.
//
// Vectors are computed on the read path and cached in process, so a model outage
// cannot fail a memory write. The cost is a cold first recall, bounded by the
// candidate cap, and re-embedding after a restart.
package hybrid

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/ionalpha/flynn/state"
)

// defaultRankConstant is the k in 1/(k+rank). 60 is the published value; a large k
// makes agreement between the lists count more than position within one.
const defaultRankConstant = 60

// defaultCandidates caps how many items each measure ranks, because the pool is read
// and embedded per recall. 200 is a guess; raise it with [WithCandidates].
const defaultCandidates = 200

// defaultDepth caps how many pool items count as matched by meaning, because the
// pool is every live item in scope and membership is no evidence of a match.
const defaultDepth = 20

// defaultCacheSize caps the vector cache in entries. Content is immutable, so a
// cached vector never goes stale.
const defaultCacheSize = 4096

// ErrNotFused reports a lexical-only ranking because the embedder failed. It is
// returned with the items, never instead of them, so a caller that only wants
// results can ignore it.
var ErrNotFused = errors.New("hybrid: ranked without embeddings")

// Embedder turns text into vectors. Embed returns one vector per text, in order.
// Vectors need not be normalized. Vectors of different lengths score 0, so a corpus
// part re-embedded under a new model degrades recall instead of failing it.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Store ranks recall by fused similarity; every other method delegates.
type Store struct {
	inner      state.MemoryStore
	emb        Embedder
	k          float64
	lexWeight  float64
	vecWeight  float64
	candidates int
	depth      int
	cache      *vecCache
}

var _ state.MemoryStore = (*Store)(nil)

// Option configures a Store.
type Option func(*Store)

// WithEmbedder sets the model. Without one, a Store passes recall through. Nil is
// ignored.
func WithEmbedder(e Embedder) Option {
	return func(s *Store) {
		if e != nil {
			s.emb = e
		}
	}
}

// WithRankConstant sets k (default 60). A smaller k favours the top of each list.
// A non-positive value is ignored.
func WithRankConstant(k float64) Option {
	return func(s *Store) {
		if k > 0 {
			s.k = k
		}
	}
}

// WithWeights sets the lexical and embedding weights (default 1 and 1). Zero turns
// one measure off. Negative weights, or two zeros, are ignored.
func WithWeights(lexical, embedding float64) Option {
	return func(s *Store) {
		if lexical < 0 || embedding < 0 || lexical+embedding == 0 {
			return
		}
		s.lexWeight, s.vecWeight = lexical, embedding
	}
}

// WithCandidates sets how many items each measure ranks (default 200), which bounds
// the pool read and the items embedded on a cold recall. A non-positive value is
// ignored.
func WithCandidates(n int) Option {
	return func(s *Store) {
		if n > 0 {
			s.candidates = n
		}
	}
}

// WithDepth sets how many pool items count as matched by meaning (default 20). A
// non-positive value is ignored; use WithWeights(1, 0) to turn meaning off.
func WithDepth(n int) Option {
	return func(s *Store) {
		if n > 0 {
			s.depth = n
		}
	}
}

// WithCacheSize caps the in-process vector cache (default 4096 entries). A
// non-positive value is ignored.
func WithCacheSize(n int) Option {
	return func(s *Store) {
		if n > 0 {
			s.cache.resize(n)
		}
	}
}

// Wrap returns a Store over inner. Without an embedder it changes nothing.
func Wrap(inner state.MemoryStore, opts ...Option) *Store {
	s := &Store{
		inner:      inner,
		k:          defaultRankConstant,
		lexWeight:  1,
		vecWeight:  1,
		candidates: defaultCandidates,
		depth:      defaultDepth,
		cache:      newVecCache(defaultCacheSize),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Recall returns memory ranked by the fused measures. It reads the inner store
// twice: once with the query, and once without the text but with every other
// selector, because a reworded fact appears only in that second pool.
//
// Both reads drop q.MinScore and use the candidate cap as the limit, because the
// inner score is not the fused score and an early limit would cut rows fusion
// promotes. Both apply to the fused list. With no query text or no embedder, Recall
// delegates; a failing embedder returns the lexical results with [ErrNotFused].
func (s *Store) Recall(ctx context.Context, q state.RecallQuery) ([]state.MemoryItem, error) {
	query := strings.TrimSpace(q.Query)
	if query == "" || s.emb == nil {
		return s.inner.Recall(ctx, q)
	}
	lexical, err := s.inner.Recall(ctx, s.innerQuery(q, query))
	if err != nil {
		return nil, err
	}
	pool, err := s.inner.Recall(ctx, s.innerQuery(q, ""))
	if err != nil {
		return nil, err
	}
	ranked, err := s.rankByMeaning(ctx, query, pool)
	if err != nil {
		return s.finish(q, lexical), fmt.Errorf("%w: %w", ErrNotFused, err)
	}
	return s.finish(q, s.fuse(lexical, ranked)), nil
}

// innerQuery builds a candidate read. The lexical read orders by relevance, which
// decides what survives the cap; the pool read keeps recency order.
func (s *Store) innerQuery(q state.RecallQuery, query string) state.RecallQuery {
	iq := q
	iq.Query = query
	iq.MinScore = 0
	iq.Limit = s.candidates
	if query != "" {
		iq.Order = state.OrderRelevance
	}
	return iq
}

// rankByMeaning returns the pool items closest to query, best first, ties broken by
// ID for determinism. Items scoring 0 or below, and those past the depth, are
// dropped, because pool membership is not a match.
func (s *Store) rankByMeaning(ctx context.Context, query string, pool []state.MemoryItem) ([]state.MemoryItem, error) {
	texts := make([]string, 0, len(pool)+1)
	texts = append(texts, query)
	for _, it := range pool {
		texts = append(texts, embedText(it))
	}
	vecs, err := s.vectors(ctx, texts)
	if err != nil {
		return nil, err
	}
	scores := make(map[string]float64, len(pool))
	for i, it := range pool {
		scores[it.ID] = cosine(vecs[0], vecs[i+1])
	}
	out := make([]state.MemoryItem, 0, len(pool))
	for _, it := range pool {
		if scores[it.ID] > 0 {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if scores[out[i].ID] != scores[out[j].ID] {
			return scores[out[i].ID] > scores[out[j].ID]
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > s.depth {
		out = out[:s.depth]
	}
	return out, nil
}

// fuse merges the rankings by ID with the fused score. An item scoring zero is
// dropped, so WithWeights(1, 0) returns only what the lexical measure found.
func (s *Store) fuse(lexical, meaning []state.MemoryItem) []state.MemoryItem {
	scores := make(map[string]float64, len(lexical)+len(meaning))
	addReciprocalRanks(scores, lexical, s.lexWeight, s.k)
	addReciprocalRanks(scores, meaning, s.vecWeight, s.k)
	// Dividing by first place in both lists puts the score on the (0,1] scale
	// MinScore expects.
	best := (s.lexWeight + s.vecWeight) / (s.k + 1)
	out := make([]state.MemoryItem, 0, len(scores))
	seen := make(map[string]bool, len(scores))
	for _, it := range append(append([]state.MemoryItem(nil), lexical...), meaning...) {
		if seen[it.ID] || scores[it.ID] == 0 {
			continue
		}
		seen[it.ID] = true
		it.Score = scores[it.ID] / best
		out = append(out, it)
	}
	return out
}

// addReciprocalRanks adds each item's weighted 1/(k+rank).
func addReciprocalRanks(scores map[string]float64, items []state.MemoryItem, weight, k float64) {
	if weight == 0 {
		return
	}
	for i, it := range items {
		scores[it.ID] += weight / (k + float64(i+1))
	}
}

// finish applies the caller's floor, order and limit, to the lexical-only fallback
// too.
func (s *Store) finish(q state.RecallQuery, items []state.MemoryItem) []state.MemoryItem {
	out := make([]state.MemoryItem, 0, len(items))
	for _, it := range items {
		if it.Score < q.MinScore {
			continue
		}
		out = append(out, it)
	}
	state.SortRecall(q, out)
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out
}

// SubjectSimilarity returns an embedding measure for curate.WithSimilarity, which
// catches synonyms such as "db-choice" and "storage-engine". It shares the recall
// cache. The score is cosine clamped to [0,1] and symmetric. A failing embedder
// scores 0, so no evidence reports no fork. Set curate.WithSimilarityThreshold to
// match.
func (s *Store) SubjectSimilarity(ctx context.Context) func(a, b string) float64 {
	return func(a, b string) float64 {
		if s.emb == nil || a == "" || b == "" {
			return 0
		}
		if a == b {
			return 1
		}
		vecs, err := s.vectors(ctx, []string{subjectText(a), subjectText(b)})
		if err != nil {
			return 0
		}
		return math.Max(0, cosine(vecs[0], vecs[1]))
	}
}

// vectors returns a vector per text, embedding the uncached ones in one call.
func (s *Store) vectors(ctx context.Context, texts []string) ([][]float32, error) {
	// Answer from this map, so a cache smaller than one call still answers it.
	have := make(map[string][]float32, len(texts))
	var missing []string
	for _, t := range texts {
		if _, ok := have[t]; ok {
			continue
		}
		if v, ok := s.cache.get(t); ok {
			have[t] = v
			continue
		}
		have[t] = nil
		missing = append(missing, t)
	}
	if len(missing) > 0 {
		vecs, err := s.emb.Embed(ctx, missing)
		if err != nil {
			return nil, err
		}
		if len(vecs) != len(missing) {
			return nil, fmt.Errorf("hybrid: embedder returned %d vectors for %d texts", len(vecs), len(missing))
		}
		for i, t := range missing {
			have[t] = vecs[i]
			s.cache.put(t, vecs[i])
		}
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = have[t]
	}
	return out, nil
}

// embedText is the subject then the content, subject first because a truncating
// model keeps what it reads first.
func embedText(it state.MemoryItem) string {
	if it.Subject == "" {
		return it.Content
	}
	return strings.TrimSpace(subjectText(it.Subject) + " " + it.Content)
}

// subjectText turns dashes into spaces, so a model reads "db choice" as words.
func subjectText(subject string) string {
	return strings.ReplaceAll(subject, "-", " ")
}

// cosine is in [-1,1]. Mismatched lengths or a zero vector score 0.
func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// vecCache holds vectors by text, evicting oldest first. Keying by text lets
// subject and item lookups share entries and embeds identical content once.
type vecCache struct {
	mu    sync.Mutex
	max   int
	vecs  map[string][]float32
	order []string
}

func newVecCache(limit int) *vecCache {
	return &vecCache{max: limit, vecs: make(map[string][]float32)}
}

func (c *vecCache) get(text string) ([]float32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.vecs[text]
	return v, ok
}

func (c *vecCache) put(text string, vec []float32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.vecs[text]; ok {
		return
	}
	c.vecs[text] = vec
	c.order = append(c.order, text)
	c.evict()
}

func (c *vecCache) resize(limit int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.max = limit
	c.evict()
}

// evict trims to the cap. The caller holds the lock.
func (c *vecCache) evict() {
	for len(c.order) > c.max {
		delete(c.vecs, c.order[0])
		c.order = c.order[1:]
	}
}

// Write delegates unchanged and embeds nothing.
func (s *Store) Write(ctx context.Context, m state.MemoryItem) (state.MemoryItem, error) {
	return s.inner.Write(ctx, m)
}

// Delete delegates unchanged.
func (s *Store) Delete(ctx context.Context, id string) error { return s.inner.Delete(ctx, id) }

// RecordPush delegates unchanged.
func (s *Store) RecordPush(ctx context.Context, memoryIDs []string) error {
	return s.inner.RecordPush(ctx, memoryIDs)
}

// RecordUse delegates unchanged.
func (s *Store) RecordUse(ctx context.Context, memoryID string, origin state.UsageOrigin) error {
	return s.inner.RecordUse(ctx, memoryID, origin)
}

// Usage delegates unchanged.
func (s *Store) Usage(ctx context.Context, memoryIDs []string) ([]state.MemoryUsage, error) {
	return s.inner.Usage(ctx, memoryIDs)
}

// Promote delegates unchanged.
func (s *Store) Promote(ctx context.Context, d state.PromotionDecision) (state.MemoryPromotion, error) {
	return s.inner.Promote(ctx, d)
}

// Promotions delegates unchanged.
func (s *Store) Promotions(ctx context.Context, memoryIDs []string) ([]state.MemoryPromotion, error) {
	return s.inner.Promotions(ctx, memoryIDs)
}
