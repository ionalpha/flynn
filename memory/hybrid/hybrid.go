// Package hybrid recalls memory by two measures at once: the word match the
// backing store already computes, and the meaning match an embedding gives, fused
// into one ranking by reciprocal rank.
//
// It exists because of how memory is actually read. A fact is written in the
// wording of the day it was learned, and asked for months later in the wording of
// the question, which shares no term with it. A lexical index answers that recall
// with nothing: the fact is stored, it is live, it is in scope, and the reader is
// told there is nothing. An embedding measure connects the two spellings and a
// lexical one cannot, so this ranks by both and lets each cover what the other
// misses. The reverse case is just as real: an identifier, an error code or a
// person's name is matched exactly by the lexical index and blurred by an
// embedding, which is why this fuses the two rather than replacing one with the
// other.
//
// Host neutrality. Nothing here computes an embedding. Embedder is the whole
// boundary: a host with a model in hand supplies one, and a host with none gets
// exactly the lexical behaviour it had before, because a Store with no embedder
// delegates every recall unchanged. No model, dimension, provider or distance
// function is named anywhere in this package.
//
// What it does not do. Vectors are computed on the read path and cached in
// process, not written to the store: a memory item is append-only and its record
// is what its writer asserted, and putting a model call in the write path would
// make writing memory fail when a model is down. The cost of that choice is a cold
// first recall, which is bounded by the candidate cap, and re-embedding after a
// restart.
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

// defaultRankConstant is the k in reciprocal rank fusion: a rank contributes
// 1/(k+rank), so k sets how sharply the top of a list beats the rest of it. 60 is
// the value the method was published with and the one every implementation since
// has used; it is deliberately large, so a list's ranks 1 and 5 differ by little
// and agreement between the two lists decides more than depth within either.
const defaultRankConstant = 60

// defaultCandidates caps how many items each measure ranks. Fusion needs a pool
// the lexical read cannot produce on its own (see Recall), and that pool is read
// and embedded per recall, so it has to be bounded by something. 200 is the
// working guess at "wide enough that the reworded fact is in it, small enough to
// embed on a read"; a host with a larger corpus or a cheaper model raises it with
// WithCandidates.
const defaultCandidates = 200

// defaultDepth caps how many of the candidate pool count as matched by meaning.
// It is what stops fusion from turning a query into "return everything": the pool
// is every live item in scope, and being in it is not evidence of anything, so
// only the closest few are treated as hits. 20 is the usual reading depth for
// rank fusion, and comfortably more than a digest or a prompt will carry.
const defaultDepth = 20

// defaultCacheSize caps the in-process vector cache. Memory content is immutable,
// so a cached vector never goes stale and the only reason to bound the cache is
// the memory it holds. 4096 entries covers the working set of a session several
// times over.
const defaultCacheSize = 4096

// ErrNotFused reports that a recall came back ranked by the lexical measure alone,
// because the embedder failed. It is returned alongside the items, never instead
// of them: the reader asked for memory and the lexical answer is the same answer
// the store gives with no embedder configured, so failing the read would trade a
// usable result for the news that it is not the better one. A caller that only
// wants results ignores an error matching this; a caller watching recall quality
// wants to know the meaning half went missing.
var ErrNotFused = errors.New("hybrid: ranked without embeddings")

// Embedder turns text into vectors. It is the only thing a host has to supply to
// get hybrid recall, and the only place a model appears in this package.
//
// Embed must return one vector per input text, in the same order. Vectors need
// not be normalized; similarity is cosine, which normalizes as it goes. Two
// vectors of different lengths score 0 against each other rather than failing the
// recall, so re-embedding a corpus under a new model degrades results until it
// finishes instead of emptying every read while it runs.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Store is a memory store that ranks recall by lexical and embedding similarity
// together. Every other operation delegates to the store it wraps.
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

// WithEmbedder sets the model that turns text into vectors. Without one, a Store
// is a pass-through: recall is exactly what the inner store returns, which is the
// honest answer for a host that has no model rather than a half-ranking from a
// substitute. A nil embedder is ignored.
func WithEmbedder(e Embedder) Option {
	return func(s *Store) {
		if e != nil {
			s.emb = e
		}
	}
}

// WithRankConstant sets the k in reciprocal rank fusion (default 60). A smaller k
// makes the top of each list dominate; a larger one makes agreement between the
// two lists matter more than position within either. A non-positive value is
// ignored, since 1/(0+rank) would make rank 1 worth as much as the rest of the
// list put together.
func WithRankConstant(k float64) Option {
	return func(s *Store) {
		if k > 0 {
			s.k = k
		}
	}
}

// WithWeights sets how much each measure counts, lexical then embedding (default
// 1 and 1). A host whose corpus is mostly identifiers and error codes leans
// lexical; one whose memory is mostly prose leans the other way. Zero on one side
// switches that measure off, which is a supported way to A/B the ranking. Negative
// weights, or two zeros, are ignored: a ranking has to be built out of something.
func WithWeights(lexical, embedding float64) Option {
	return func(s *Store) {
		if lexical < 0 || embedding < 0 || lexical+embedding == 0 {
			return
		}
		s.lexWeight, s.vecWeight = lexical, embedding
	}
}

// WithCandidates sets how many items each measure ranks before fusion (default
// 200). It bounds both the pool read from the store and the number of items
// embedded on a cold recall. A non-positive value is ignored.
func WithCandidates(n int) Option {
	return func(s *Store) {
		if n > 0 {
			s.candidates = n
		}
	}
}

// WithDepth sets how many of the candidate pool count as matched by meaning
// (default 20). Raising it lets a weaker embedding match still earn a place in
// the fused ranking; lowering it makes the meaning half stricter. A non-positive
// value is ignored: a depth of zero is WithWeights(1, 0) said obscurely.
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

// Wrap returns a Store over inner. With no embedder it delegates every recall
// unchanged, so wrapping is safe before a host has a model to give it.
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

// Recall returns memory ranked by the two measures fused, capped at q.Limit.
//
// It reads the inner store twice, because fusion needs a list the lexical read
// cannot produce. The first read is the query as asked, ranked by the store's own
// relevance. The second drops the query text and keeps every other selector,
// which is the candidate pool: the items that are live, in scope, and of the kinds
// and anchors asked for, whether or not they share a word with the question. The
// reworded fact is in the second list and absent from the first, which is the
// entire case this package exists for; taking the pool from the lexical hits
// instead would only re-rank what the lexical measure already found. Being in the
// pool is not a match, though, so only its closest items to the query count as
// matched by meaning: see rankByMeaning and WithDepth.
//
// Both reads drop q.MinScore and take q.Limit up to the candidate cap. A floor is
// a floor on the fused score, and the two inner scales are not it; a limit applied
// before fusion would cut the rows fusion was about to promote. Both are applied
// here, to the fused ranking.
//
// A recall with no query text, or one on a Store with no embedder, delegates
// unchanged: there is nothing to fuse. An embedder that fails returns the lexical
// results with ErrNotFused.
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

// innerQuery is the recall the inner store is asked for: q with the text replaced,
// the floor and ordering set for a candidate list, and the limit widened to the
// candidate cap. Relevance order matters on the lexical read, where it decides
// which rows survive the cap; on the pool read the store has nothing to rank by
// and returns its recency order, which is the right thing to truncate by when the
// pool is bigger than the cap.
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

// rankByMeaning returns the pool's closest items to the query by cosine
// similarity, best first, with the item ID as the tiebreak so equal scores rank
// deterministically.
//
// Two things cut the list down, and both are the same point: being in the pool is
// not a match. An item scoring zero or below shares no meaning with the query at
// all and is dropped outright, and past the reading depth an item is only nearer
// than the rest of a corpus that has nothing to do with the question.
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

// fuse combines the two rankings by reciprocal rank and returns one deduplicated
// list carrying the fused score. Items are keyed by ID, so an item in both lists
// appears once with the score both ranks earned it.
//
// An item scoring zero is dropped rather than returned at the bottom. That is what
// a switched-off measure means: WithWeights(1, 0) has to return what the lexical
// measure found and nothing else, not the whole candidate pool ranked behind it.
func (s *Store) fuse(lexical, meaning []state.MemoryItem) []state.MemoryItem {
	scores := make(map[string]float64, len(lexical)+len(meaning))
	addReciprocalRanks(scores, lexical, s.lexWeight, s.k)
	addReciprocalRanks(scores, meaning, s.vecWeight, s.k)
	// The best a single item can score is first place in both lists, so dividing by
	// that puts the fused score on the (0,1] scale state.MemoryItem.Score is defined
	// in and keeps a MinScore floor meaningful.
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

// addReciprocalRanks adds each item's 1/(k+rank) contribution, weighted. A weight
// of zero adds nothing, which is how a measure is switched off.
func addReciprocalRanks(scores map[string]float64, items []state.MemoryItem, weight, k float64) {
	if weight == 0 {
		return
	}
	for i, it := range items {
		scores[it.ID] += weight / (k + float64(i+1))
	}
}

// finish applies the caller's floor, ordering and limit to a fused list. It is
// also the path the degraded answer takes, so a lexical-only result is ordered and
// capped the same way a fused one is.
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

// SubjectSimilarity returns a subject measure backed by the same embedder and the
// same cache, for memory/curate's fork detection (curate.WithSimilarity). The
// lexical measure curate ships with cannot see through a synonym: "db-choice" and
// "storage-engine" are one topic that shares no token. This can, and sharing the
// cache means a subject already embedded on a recall costs nothing on the write.
//
// The score is cosine clamped to [0,1] and is symmetric, as that option requires.
// A failing embedder scores 0, which reports no fork: the measure decides whether
// to tell somebody two subjects look alike, and having no evidence is not evidence.
// Pass a matching threshold with curate.WithSimilarityThreshold, since a score only
// means something against the function that produced it.
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

// vectors returns a vector per text, embedding only the ones not already cached
// and doing so in one call. Identical texts are embedded once.
func (s *Store) vectors(ctx context.Context, texts []string) ([][]float32, error) {
	// The answer is assembled from this map rather than read back out of the cache,
	// so a cache too small to hold one call's texts still answers that call
	// correctly and only loses the entries for the next one.
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

// embedText is what an item is embedded as: its subject, then its content. The
// subject is a slug, so its dashes become spaces, and it goes first because it is
// the item's topic in the writer's own words and a truncating model keeps what it
// reads first.
func embedText(it state.MemoryItem) string {
	if it.Subject == "" {
		return it.Content
	}
	return strings.TrimSpace(subjectText(it.Subject) + " " + it.Content)
}

// subjectText renders a subject slug as the phrase it stands for, so a model reads
// "db choice" rather than one unknown token.
func subjectText(subject string) string {
	return strings.ReplaceAll(subject, "-", " ")
}

// cosine is the similarity between two vectors, in [-1,1]. Vectors of different
// lengths, or a zero vector, score 0: a corpus half re-embedded under a new model
// should degrade, not fail.
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

// vecCache holds embeddings by the text they were computed from, bounded by
// entry count and evicting in insertion order. Keying by text rather than by item
// ID means the write path's subject lookups and the read path's item lookups share
// one cache, and two items with identical content are embedded once.
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

// evict drops the oldest entries until the cache is within its cap. The caller
// holds the lock.
func (c *vecCache) evict() {
	for len(c.order) > c.max {
		delete(c.vecs, c.order[0])
		c.order = c.order[1:]
	}
}

// Write delegates unchanged. Nothing is embedded here: see the package comment on
// why a model call has no business in the write path.
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
