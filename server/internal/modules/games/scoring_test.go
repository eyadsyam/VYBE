package games

import (
	"fmt"
	"math/big"
	"testing"
	"time"
)

var t0 = time.Date(2026, 8, 25, 20, 30, 0, 0, time.UTC)

// openQuestion builds a live question with a 20s limit and 100 base points.
func openQuestion() Question {
	return Question{
		ID:              "q1",
		CorrectOptionID: "opt-correct",
		Points:          100,
		TimeLimit:       20 * time.Second,
		OpenedAt:        t0,
		DeadlineAt:      t0.Add(20 * time.Second),
	}
}

func validContext() Context {
	return Context{
		SessionActive: true,
		IsParticipant: true,
		Question:      openQuestion(),
		ExpectedNonce: "nonce-for-user-a",
	}
}

func validSubmission() Submission {
	return Submission{
		SessionID:  "s1",
		UserID:     "user-a",
		QuestionID: "q1",
		OptionID:   "opt-correct",
		Nonce:      "nonce-for-user-a",
		ReceivedAt: t0.Add(5 * time.Second),
		ClientTS:   t0.Add(5 * time.Second),
	}
}

// ===========================================================================
// SPEC-001 §5 — the ten §8.2 anti-cheat cases, as a table.
// Each row is an attack; each expectation is the defence.
// ===========================================================================

func TestValidate_AntiCheatSuite(t *testing.T) {
	cases := []struct {
		name    string
		ac      string
		mutate  func(*Submission, *Context)
		want    RejectReason
		wantHTTP int
	}{
		{
			name: "AC-14 §8.2#1: submitting the same answer twice",
			ac:   "AC-14",
			mutate: func(_ *Submission, c *Context) {
				c.AlreadyAnswered = true
			},
			want: RejectDuplicate, wantHTTP: 409,
		},
		{
			name: "AC-15 §8.2#2: submitted after deadline + grace",
			ac:   "AC-15",
			mutate: func(s *Submission, c *Context) {
				// 500ms late — beyond the 400ms transit grace.
				s.ReceivedAt = c.Question.DeadlineAt.Add(500 * time.Millisecond)
			},
			want: RejectQuestionClosed, wantHTTP: 422,
		},
		{
			name: "AC-16 §8.2#3: question belongs to another session",
			ac:   "AC-16",
			mutate: func(_ *Submission, c *Context) {
				// The handler resolves the question WITHIN the caller's
				// session; a foreign question resolves to nothing.
				c.Question = Question{}
			},
			want: RejectQuestionNotOpen, wantHTTP: 404,
		},
		{
			name: "AC-17 §8.2#4: replaying another user's nonce",
			ac:   "AC-17",
			mutate: func(s *Submission, _ *Context) {
				s.Nonce = "nonce-for-user-b"
			},
			want: RejectInvalidNonce, wantHTTP: 403,
		},
		{
			name: "§8.2#4b: empty nonce cannot match an unissued nonce",
			ac:   "AC-17",
			mutate: func(s *Submission, c *Context) {
				s.Nonce, c.ExpectedNonce = "", ""
			},
			want: RejectInvalidNonce, wantHTTP: 403,
		},
		{
			name: "AC-21 §8.2#8: question was never opened",
			ac:   "AC-21",
			mutate: func(_ *Submission, c *Context) {
				c.Question.OpenedAt = time.Time{}
			},
			want: RejectQuestionNotOpen, wantHTTP: 404,
		},
		{
			name: "AC-22 §8.2#9: submitter is not a participant",
			ac:   "AC-22",
			mutate: func(_ *Submission, c *Context) {
				c.IsParticipant = false
			},
			want: RejectNotParticipant, wantHTTP: 403,
		},
		{
			name: "session already completed",
			ac:   "FR-47a",
			mutate: func(_ *Submission, c *Context) {
				c.SessionActive = false
			},
			want: RejectSessionInactive, wantHTTP: 422,
		},
		{
			name: "question already closed",
			ac:   "FR-47c",
			mutate: func(_ *Submission, c *Context) {
				c.Question.ClosedAt = t0.Add(21 * time.Second)
			},
			want: RejectQuestionClosed, wantHTTP: 422,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sub, ctx := validSubmission(), validContext()
			c.mutate(&sub, &ctx)

			got := Validate(sub, ctx)
			if got.Rejected != c.want {
				t.Fatalf("[%s] rejected = %q, want %q", c.ac, got.Rejected, c.want)
			}
			if got.Accepted() {
				t.Fatalf("[%s] Accepted() must be false when rejected", c.ac)
			}
			if got.Points != 0 {
				t.Fatalf("[%s] a rejected answer awarded %d points", c.ac, got.Points)
			}
			if status := got.Rejected.HTTPStatus(); status != c.wantHTTP {
				t.Fatalf("[%s] status = %d, want %d", c.ac, status, c.wantHTTP)
			}
		})
	}
}

// AC-15 boundary: the grace exists to forgive NETWORK TRANSIT, so exactly at
// the grace edge must still be accepted, and one nanosecond past must not.
func TestValidate_GraceBoundaryIsExact(t *testing.T) {
	t.Run("exactly at deadline + grace is accepted", func(t *testing.T) {
		sub, ctx := validSubmission(), validContext()
		sub.ReceivedAt = ctx.Question.DeadlineAt.Add(AnswerGrace)
		if got := Validate(sub, ctx); !got.Accepted() {
			t.Fatalf("rejected at the exact grace edge: %q", got.Rejected)
		}
	})

	t.Run("one nanosecond past grace is rejected", func(t *testing.T) {
		sub, ctx := validSubmission(), validContext()
		sub.ReceivedAt = ctx.Question.DeadlineAt.Add(AnswerGrace + time.Nanosecond)
		if got := Validate(sub, ctx); got.Rejected != RejectQuestionClosed {
			t.Fatalf("rejected = %q, want QUESTION_CLOSED", got.Rejected)
		}
	})
}

// ===========================================================================
// AC-18 / §8.2#5 — the device clock is irrelevant.
// ===========================================================================

// The headline anti-cheat claim of ADR-004: a manipulated device clock changes
// nothing, because nothing in the scoring path reads one.
func TestValidate_DeviceClockCannotAffectScore(t *testing.T) {
	honest := validSubmission()
	honestResult := Validate(honest, validContext())
	if !honestResult.Accepted() {
		t.Fatalf("baseline submission rejected: %q", honestResult.Rejected)
	}

	skews := []time.Duration{
		-60 * time.Second, // §8.2#5 verbatim
		-24 * time.Hour,
		+24 * time.Hour,
		-5 * time.Minute,
	}

	for _, skew := range skews {
		t.Run(fmt.Sprintf("clock skewed by %v", skew), func(t *testing.T) {
			cheat := validSubmission()
			cheat.ClientTS = cheat.ReceivedAt.Add(skew) // the only thing they control

			got := Validate(cheat, validContext())
			if got.Points != honestResult.Points {
				t.Fatalf("skewing the device clock by %v changed the score from %d to %d",
					skew, honestResult.Points, got.Points)
			}
			if got.Elapsed != honestResult.Elapsed {
				t.Fatalf("skewing the device clock changed elapsed from %v to %v",
					honestResult.Elapsed, got.Elapsed)
			}
		})
	}
}

// ===========================================================================
// AC-24 / AC-25 — the scoring formula.
// ===========================================================================

func TestScore(t *testing.T) {
	const limit = 20 * time.Second

	cases := []struct {
		name    string
		base    int
		elapsed time.Duration
		want    int
		why     string
	}{
		{
			name: "AC-24: base 100, limit 20s, elapsed 5s", base: 100,
			elapsed: 5 * time.Second, want: 138,
			why: "100 + round(100*0.5*0.75) = 100 + round(37.5) = 138",
		},
		{
			name: "instant answer earns the full bonus", base: 100,
			elapsed: 0, want: 150,
			why: "100 + round(100*0.5*1.0)",
		},
		{
			name: "halfway through earns half the bonus", base: 100,
			elapsed: 10 * time.Second, want: 125,
			why: "100 + round(100*0.5*0.5)",
		},
		{
			name: "at the limit earns base only", base: 100,
			elapsed: limit, want: 100,
			why: "correct but out of time",
		},
		{
			name: "past the limit still earns base", base: 100,
			elapsed: 30 * time.Second, want: 100,
			why: "grace-accepted answers score base, never negative",
		},
		{
			name: "rounds half up, not down", base: 3,
			elapsed: 0, want: 5,
			why: "3 + round(1.5) = 3 + 2",
		},
		{
			name: "zero base scores zero", base: 0,
			elapsed: 0, want: 0,
			why: "no points configured",
		},
		{
			name: "negative base is refused", base: -50,
			elapsed: 0, want: 0,
			why: "content bug must not produce negative XP",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Score(c.base, c.elapsed, limit); got != c.want {
				t.Fatalf("Score(%d, %v, %v) = %d, want %d — %s",
					c.base, c.elapsed, limit, got, c.want, c.why)
			}
		})
	}

	t.Run("zero time limit means no speed component", func(t *testing.T) {
		if got := Score(100, 5*time.Second, 0); got != 100 {
			t.Fatalf("got %d, want 100", got)
		}
	})

	t.Run("negative elapsed is clamped, never a super-bonus", func(t *testing.T) {
		if got := Score(100, -5*time.Second, limit); got != 150 {
			t.Fatalf("got %d, want 150 (the maximum, not more)", got)
		}
	})
}

// The integer formula must track the §8.1 definition exactly across the whole
// domain, not just at the sampled points above.
//
// The reference is computed with exact rationals rather than float64. That is
// not pedantry — it is the point. Written as float64, this check disagrees at
// base=250, elapsed=10.96s: the true bonus is exactly 56.5, but
// 1 - 10960.0/20000.0 evaluates to 0.45199999999999996, so the float reference
// rounds down to 56 while the exact answer rounds up to 57.
//
// That is the float accumulation §6.1 rule 4 exists to avoid, and the reason
// two servers computing the same answer in float could disagree about a user's
// score. The implementation is integer-only; so is this check.
func TestScore_MatchesTheSpecFormulaEverywhere(t *testing.T) {
	const limit = 20 * time.Second

	// half is added before flooring to get round-half-up.
	half := big.NewRat(1, 2)

	for _, base := range []int{1, 3, 7, 50, 100, 250, 999} {
		for ms := 0; ms <= 20000; ms += 137 {
			elapsed := time.Duration(ms) * time.Millisecond

			// bonus = base * 0.5 * (1 - elapsed/limit), exactly.
			ratio := new(big.Rat).SetFrac64(int64(limit-elapsed), int64(limit))
			bonus := new(big.Rat).Mul(big.NewRat(int64(base), 2), ratio)
			bonus.Add(bonus, half)
			want := base + int(new(big.Int).Div(bonus.Num(), bonus.Denom()).Int64())

			if got := Score(base, elapsed, limit); got != want {
				t.Fatalf("Score(%d, %v, %v) = %d, want %d (exact spec formula)",
					base, elapsed, limit, got, want)
			}
		}
	}
}

// Pin the specific case that exposed the float discrepancy, so a future
// "simplification" back to float64 fails here with an explanation.
func TestScore_ExactHalfRoundsUp(t *testing.T) {
	// base 250, 10.96s of 20s: bonus is exactly 250 * 0.5 * 0.452 = 56.5.
	got := Score(250, 10960*time.Millisecond, 20*time.Second)
	if want := 307; got != want {
		t.Fatalf("Score = %d, want %d — an exact .5 bonus must round UP; "+
			"getting %d means the calculation went through float64", got, want, want-1)
	}
}

// Score must never exceed base * 1.5 rounded up — leaderboard arithmetic and
// the §8.4 daily caps both assume a bounded per-question maximum.
func TestScore_NeverExceedsMaxScore(t *testing.T) {
	const limit = 20 * time.Second
	for _, base := range []int{1, 2, 3, 100, 999, 1000} {
		max := MaxScore(base)
		for ms := -5000; ms <= 25000; ms += 251 {
			got := Score(base, time.Duration(ms)*time.Millisecond, limit)
			if got > max {
				t.Fatalf("Score(%d, %dms) = %d exceeds MaxScore = %d", base, ms, got, max)
			}
			if got < 0 {
				t.Fatalf("Score(%d, %dms) = %d is negative", base, ms, got)
			}
		}
	}
}

// ===========================================================================
// RTT compensation — the fairness property.
// ===========================================================================

func TestElapsed_RTTCompensation(t *testing.T) {
	t.Run("half the RTT is deducted", func(t *testing.T) {
		got := Elapsed(t0, t0.Add(5*time.Second), 400*time.Millisecond)
		if want := 4800 * time.Millisecond; got != want {
			t.Fatalf("Elapsed = %v, want %v (5s minus half of 400ms)", got, want)
		}
	})

	// AC-25: a large RTT against a fast answer must clamp, not go negative.
	t.Run("AC-25: negative elapsed clamps to zero", func(t *testing.T) {
		if got := Elapsed(t0, t0.Add(100*time.Millisecond), 2*time.Second); got != 0 {
			t.Fatalf("Elapsed = %v, want 0", got)
		}
	})

	// The fairness claim of §8.1, made concrete: two users who pressed the
	// button at the same moment must score the same, whatever their network.
	t.Run("slow network is not penalised versus fast", func(t *testing.T) {
		const thinkTime = 4 * time.Second

		fastRTT, slowRTT := 30*time.Millisecond, 900*time.Millisecond

		// Each user's packet arrives thinkTime + one network leg later.
		fast := Elapsed(t0, t0.Add(thinkTime+fastRTT/2), fastRTT)
		slow := Elapsed(t0, t0.Add(thinkTime+slowRTT/2), slowRTT)

		if fast != slow {
			t.Fatalf("same think time scored differently: fast %v vs slow %v", fast, slow)
		}
		if fast != thinkTime {
			t.Fatalf("compensated elapsed = %v, want the true think time %v", fast, thinkTime)
		}

		// And without compensation the slow user would have lost real points —
		// proving the compensation is doing work.
		uncompensated := Elapsed(t0, t0.Add(thinkTime+slowRTT/2), 0)
		if Score(100, uncompensated, 20*time.Second) >= Score(100, slow, 20*time.Second) {
			t.Fatal("test does not exercise the compensation: it changed no score")
		}
	})
}

// ===========================================================================
// §8.4 abuse signalling — score, do not punish.
// ===========================================================================

func TestValidate_SubHumanLatencyIsFlaggedButStillCounts(t *testing.T) {
	sub, ctx := validSubmission(), validContext()
	sub.ReceivedAt = t0.Add(80 * time.Millisecond) // faster than any human

	got := Validate(sub, ctx)
	if !got.Accepted() {
		t.Fatalf("§8.4 says score, do not punish — the answer must still count, got %q", got.Rejected)
	}
	if !got.Suspicious {
		t.Fatal("latency below the human floor must be flagged for review")
	}
	if got.Points == 0 {
		t.Fatal("a flagged answer still scores; withholding happens at the ledger, not here")
	}
}

func TestValidate_NormalLatencyIsNotFlagged(t *testing.T) {
	if got := Validate(validSubmission(), validContext()); got.Suspicious {
		t.Fatal("a 5-second answer must not be flagged as suspicious")
	}
}

// ===========================================================================
// Correctness and ordering
// ===========================================================================

func TestValidate_WrongAnswerScoresZeroButIsAccepted(t *testing.T) {
	sub, ctx := validSubmission(), validContext()
	sub.OptionID = "opt-wrong"

	got := Validate(sub, ctx)
	if !got.Accepted() {
		t.Fatalf("a wrong answer is still a valid submission, got %q", got.Rejected)
	}
	if got.IsCorrect {
		t.Fatal("IsCorrect must be false")
	}
	if got.Points != 0 {
		t.Fatalf("wrong answer awarded %d points", got.Points)
	}
	if got.Elapsed == 0 {
		t.Fatal("elapsed must still be recorded for a wrong answer (§8.4 signals)")
	}
}

// Check order is part of the security design: a non-participant probing
// question IDs must get 403 whether or not the ID exists, so status codes
// cannot be used to enumerate content.
func TestValidate_MembershipCheckedBeforeQuestionLookup(t *testing.T) {
	sub, ctx := validSubmission(), validContext()
	ctx.IsParticipant = false
	ctx.Question = Question{} // question does not exist either

	if got := Validate(sub, ctx); got.Rejected != RejectNotParticipant {
		t.Fatalf("rejected = %q, want NOT_PARTICIPANT — a 404 here would confirm "+
			"to an outsider that the question ID does not exist", got.Rejected)
	}
}

func TestRejectReason_HTTPStatusIsTotal(t *testing.T) {
	all := []RejectReason{
		RejectNone, RejectSessionInactive, RejectNotParticipant,
		RejectQuestionNotOpen, RejectQuestionClosed, RejectInvalidNonce,
		RejectDuplicate,
	}
	for _, r := range all {
		if got := r.HTTPStatus(); got < 200 || got >= 500 {
			t.Fatalf("RejectReason(%q).HTTPStatus() = %d, want a 2xx/4xx", r, got)
		}
	}
	if got := RejectReason("SOMETHING_NEW").HTTPStatus(); got != 500 {
		t.Fatalf("an unmapped reason must fail loudly as 500, got %d", got)
	}
}

func TestMaxScore(t *testing.T) {
	cases := map[int]int{0: 0, -5: 0, 1: 2, 2: 3, 3: 5, 100: 150, 999: 1499}
	for base, want := range cases {
		if got := MaxScore(base); got != want {
			t.Fatalf("MaxScore(%d) = %d, want %d", base, got, want)
		}
	}
}
