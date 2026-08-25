# ADR-007: A two-stage heuristic ranker behind a `Ranker` seam

Status: Accepted
Date: 2026-08-25

## Context

§0.3 forbids claiming "AI-powered recommendations" with no model, no evaluation,
and no offline metric — it is named in §0.6 as a specific tell of a generated
portfolio project. At V1 the system has essentially no interaction data: a
handful of seeded users and 200 titles. A learned model trained on that would be
noise wearing a lab coat.

The honest question is therefore not "which model", but "what can be built now
that is genuinely evaluated, and what seam lets a model replace it later without
a rewrite".

## Options considered

| Option | Pros | Cons |
|---|---|---|
| Popularity only | Trivial, strong cold-start baseline, honest | No personalisation; nothing to defend beyond `ORDER BY` |
| **Two-stage: candidate generation, then a weighted linear ranker** | Personalised, explainable per item, cheap, measurable offline, and structurally identical to how real systems are built | Weights are hand-chosen until tuned; it may not beat popularity by much at V1 data volume |
| Collaborative filtering (matrix factorisation) | Genuinely learned | Needs interaction volume we do not have; cold-starts badly; unevaluable at V1 scale |
| Embedding / LLM re-ranking | Fashionable | Latency and cost per feed load, no offline harness, unexplainable to a user, and impossible to defend on 200 titles |

## Decision

**Two-stage retrieval and ranking, with the V1 ranker being a documented,
offline-evaluated weighted linear function** (§10.2), sitting behind an
interface:

```go
type Ranker interface {
    Rank(ctx context.Context, candidates []Candidate,
         features UserFeatures, rc RankContext) ([]Ranked, error)
}
```

Stage 1 generates ~500 candidates from six pools (trending, genre affinity,
friends watching, continue watching, similar-to-recent, editorial). Stage 2
scores to ~50. Stage 3 re-ranks for diversity with MMR at lambda 0.7, caps a
genre at 3 consecutive items and 40% of a shelf, and reserves 10% of slots for
epsilon-greedy exploration.

**The seam matters more than the algorithm.** Swapping in a learned model later
touches one implementation of one interface.

Two rules keep this honest:

1. **Weights live in configuration, not code**, and are labelled a hypothesis
   until the §10.4 harness says otherwise.
2. **The ranker must beat global popularity on nDCG@10** in the offline harness,
   against baselines of random, popularity, and genre-only. *If it does not, the
   documentation says so in those words.* A heuristic that fails to beat
   popularity is not earning its complexity, and pretending otherwise is exactly
   the fabrication §0.3 prohibits.

Every recommended item carries a machine-readable `reason`
(`because_you_watched:{id}`, `friends_watching`, `trending_in_region`,
`editorial`), surfaced in the UI. This makes the system debuggable and the
recommendation trustworthy.

## Consequences

**Becomes easy**

- Every ranked item is explainable — you can point at which term dominated.
- Tuning is a config change plus a harness run, not a deploy.
- Replacing the ranker with a model is a one-class change.

**Becomes hard**

- The offline harness (replay, hold out last 20%, compute Precision@10,
  Recall@50, nDCG@10, catalogue coverage, intra-list diversity) is real work
  that must exist before any weight is called "tuned".
- Seeded data must be plausible enough for the harness to mean anything. §16.3's
  "40 users with realistic social graphs and watch histories" is a hard
  requirement here, not decoration.

**Revisit when** there are enough real interactions to train and *validate* a
model — order 10^5 interactions, not 10^3.
