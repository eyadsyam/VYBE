// Package games owns trivia and (from V1.5) predictions.
//
// Everything in this file is pure. That is deliberate: the anti-cheat rules of
// §8.1 are the highest-value logic in the system and the easiest to get subtly
// wrong, so they are written as functions over values with no clock, no
// database, and no network. The ten attacks in §8.2 become ten table entries.
//
// ADR-004 governs: the client has zero authority. Every input to this package
// that could be attacker-controlled is named so at its declaration.
package games

import (
	"time"
)

// AnswerGrace is the tolerance past a question's deadline within which an
// answer is still accepted (§8.1 step 5c).
//
// It exists to compensate NETWORK TRANSIT, not to be generous: a user on a
// slow link pressed the button in time, and the packet took 300ms. Predictions
// deliberately get no equivalent (§8.3) — there, arriving late means having had
// information the others did not, which is the cheat itself.
const AnswerGrace = 400 * time.Millisecond

// HumanReactionFloor is the fastest a human plausibly reads a question and
// taps an option. Consistently beating it is an abuse SIGNAL, not proof, and
// per §8.4 it results in shadow-scoring pending review — never an auto-ban.
const HumanReactionFloor = 250 * time.Millisecond

// RejectReason enumerates every way an answer can be refused. Each maps to a
// stable machine-readable `code` in the RFC 9457 response (§5.2), because
// clients branch on these and a renamed string is a breaking change.
type RejectReason string

const (
	RejectNone            RejectReason = ""
	RejectSessionInactive RejectReason = "SESSION_INACTIVE"
	RejectNotParticipant  RejectReason = "NOT_PARTICIPANT"
	RejectQuestionNotOpen RejectReason = "QUESTION_NOT_FOUND"
	RejectQuestionClosed  RejectReason = "QUESTION_CLOSED"
	RejectInvalidNonce    RejectReason = "INVALID_NONCE"
	RejectDuplicate       RejectReason = "DUPLICATE_ANSWER"
)

// HTTPStatus maps a rejection to its response status. Kept next to the reason
// so the two cannot drift apart.
func (r RejectReason) HTTPStatus() int {
	switch r {
	case RejectNone:
		return 200
	case RejectSessionInactive, RejectQuestionClosed:
		return 422
	case RejectNotParticipant, RejectInvalidNonce:
		return 403
	case RejectQuestionNotOpen:
		return 404
	case RejectDuplicate:
		return 409
	}
	return 500
}

// Question is the server's full view, including the answer key.
//
// Nothing in this struct is ever serialised to a client before
// QUESTION_CLOSE. The outbound projection is built explicitly in the
// transport layer and is guarded by AC-20, which serialises the payload and
// asserts the key does not appear in it.
type Question struct {
	ID              string
	CorrectOptionID string
	Points          int
	TimeLimit       time.Duration
	OpenedAt        time.Time // server clock at broadcast — the ONLY timing source
	DeadlineAt      time.Time
	ClosedAt        time.Time // zero while open
}

// Open reports whether the question is currently accepting answers, ignoring
// grace. Grace is applied in Validate, where the receipt time is known.
func (q Question) Open() bool {
	return !q.OpenedAt.IsZero() && q.ClosedAt.IsZero()
}

// Submission carries everything needed to judge one answer.
//
// The field comments matter more than the types here: they mark the trust
// boundary of §12.2 inline, so that a future edit that starts scoring from
// ClientTS is visibly wrong at the point of the change.
type Submission struct {
	SessionID  string
	UserID     string
	QuestionID string
	OptionID   string

	// Nonce as presented by the client — ATTACKER-CONTROLLED. Compared against
	// ExpectedNonce; never used for anything else.
	Nonce string

	// ReceivedAt is the server's own clock reading at the moment the request
	// arrived. THIS IS THE AUTHORITATIVE TIMING SOURCE (§8.1 step 6).
	ReceivedAt time.Time

	// ClientTS is what the device claimed the time was — ATTACKER-CONTROLLED.
	// Recorded as telemetry only. Its divergence from ReceivedAt is an abuse
	// signal (§8.4). It MUST NOT influence scoring, and no function in this
	// package reads it.
	ClientTS time.Time

	// RTT as measured by the server's own PING/PONG, not reported by the
	// client. A client-supplied RTT would be a free speed bonus.
	RTT time.Duration
}

// Context is the server-side state the submission is judged against. Every
// field is read from the database or from server memory — none of it comes
// from the request.
type Context struct {
	SessionActive bool
	IsParticipant bool
	Question      Question

	// ExpectedNonce is the nonce THIS server issued to THIS user for THIS
	// question. Empty means none was issued, which is itself a rejection.
	ExpectedNonce string

	// AlreadyAnswered reflects a prior row. It is a fast-path check only: the
	// real guarantee is the UNIQUE constraint on
	// (session_id, user_id, question_id), because two concurrent requests can
	// both read false here and only one can win the insert (AC-19).
	AlreadyAnswered bool
}

// Result is the outcome of judging a submission.
type Result struct {
	Rejected  RejectReason
	IsCorrect bool
	Points    int

	// Elapsed is post-RTT-compensation and post-clamp. Persisted so scoring is
	// auditable after the fact and so §8.4's latency-based abuse signals have
	// something to read.
	Elapsed time.Duration

	// Suspicious flags a below-human-floor latency. §8.4: score, do not
	// punish. The answer still counts; the user is shadow-scored pending
	// review.
	Suspicious bool
}

// Accepted reports whether the submission was recorded.
func (r Result) Accepted() bool { return r.Rejected == RejectNone }

// Validate applies §8.1 step 5 in order, and scores when the answer stands.
//
// The ORDER of these checks is part of the security design, not a style
// choice. Membership and session state are checked before anything derived
// from the question, so that a non-participant probing question IDs learns
// nothing about which IDs exist — the response is 403 either way, never a 404
// that confirms a guess.
func Validate(sub Submission, ctx Context) Result {
	// a. Is there a live round at all?
	if !ctx.SessionActive {
		return Result{Rejected: RejectSessionInactive}
	}

	// b. Is this user in the room? Before question lookup, so a stranger
	//    cannot enumerate question IDs by status code.
	if !ctx.IsParticipant {
		return Result{Rejected: RejectNotParticipant}
	}

	// c. Was this question ever opened in this session?
	if !ctx.Question.Open() {
		if ctx.Question.OpenedAt.IsZero() {
			return Result{Rejected: RejectQuestionNotOpen} // AC-21
		}
		return Result{Rejected: RejectQuestionClosed}
	}

	// d. Nonce. Constant-time comparison is unnecessary — a nonce is
	//    single-use and per-user, so a timing oracle yields nothing an
	//    attacker cannot already enumerate. Length is checked first so an
	//    empty ExpectedNonce can never match an empty presented nonce.
	if ctx.ExpectedNonce == "" || sub.Nonce != ctx.ExpectedNonce {
		return Result{Rejected: RejectInvalidNonce} // AC-17
	}

	// e. Fast-path duplicate check. The authoritative guard is the database
	//    constraint; this only avoids doing the work when we already know.
	if ctx.AlreadyAnswered {
		return Result{Rejected: RejectDuplicate} // AC-14
	}

	// f. Deadline, measured on the SERVER clock plus transit grace.
	//    A device clock set sixty seconds back changes nothing here, because
	//    nothing here reads a device clock (AC-18).
	if sub.ReceivedAt.After(ctx.Question.DeadlineAt.Add(AnswerGrace)) {
		return Result{Rejected: RejectQuestionClosed} // AC-15
	}

	elapsed := Elapsed(ctx.Question.OpenedAt, sub.ReceivedAt, sub.RTT)
	correct := sub.OptionID == ctx.Question.CorrectOptionID

	res := Result{
		IsCorrect:  correct,
		Elapsed:    elapsed,
		Suspicious: elapsed < HumanReactionFloor,
	}
	if correct {
		res.Points = Score(ctx.Question.Points, elapsed, ctx.Question.TimeLimit)
	}
	return res
}

// Elapsed computes how long the user actually took, per §8.1 step 6:
//
//	elapsed = server_receive_time − question_open_time − (rtt/2)
//
// The RTT/2 term is the half that matters for fairness. Without it, a user in
// Aswan on 3G is penalised against a user on fibre for the network, not for
// their knowledge — and §8.1 calls that out as "the detail that shows you
// thought about real users".
//
// Clamped at zero: a large RTT estimate against a fast answer can go negative,
// and a negative elapsed would produce a speed bonus above the maximum.
func Elapsed(openedAt, receivedAt time.Time, rtt time.Duration) time.Duration {
	elapsed := receivedAt.Sub(openedAt) - rtt/2
	if elapsed < 0 {
		return 0 // AC-25
	}
	return elapsed
}

// Score implements §8.1 step 6:
//
//	base        = question.points
//	speed_bonus = round(base * 0.5 * max(0, 1 - elapsed/time_limit))
//
// Integer arithmetic throughout (§6.1 rule 4: money and scores are integers,
// never floats). Rounding is half-up on a non-negative value, so it is both
// deterministic across platforms and free of the float accumulation that would
// make two servers disagree about the same answer.
func Score(base int, elapsed, timeLimit time.Duration) int {
	if base <= 0 {
		return 0
	}
	if timeLimit <= 0 {
		return base // no time limit means no speed component
	}
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed >= timeLimit {
		return base // out of time, but correct: full base, no bonus
	}

	// remaining/timeLimit as a rational, scaled to avoid floats entirely.
	remaining := int64(timeLimit - elapsed)
	limit := int64(timeLimit)

	// bonus = base * 0.5 * remaining/limit = (base * remaining) / (2 * limit)
	//
	// Rounded half-up by adding half the denominator before the integer
	// division: floor((n + d/2) / d) == round-half-up(n/d) for non-negative n.
	// Here d = 2*limit, so d/2 = limit.
	bonus := (int64(base)*remaining + limit) / (2 * limit)

	return base + int(bonus)
}

// MaxScore is the most a single question can award: base plus the full speed
// bonus. Used to sanity-check content and to bound leaderboard arithmetic.
func MaxScore(base int) int {
	if base <= 0 {
		return 0
	}
	return base + (base+1)/2
}
