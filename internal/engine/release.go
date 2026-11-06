package engine

import "time"

// ReleaseCheck is the outcome of the automatic release rules for one reserved rep.
type ReleaseCheck struct {
	Release  bool   `json:"release"`
	AskFirst bool   `json:"ask_first"` // stuck: ask the junior before releasing
	Reason   string `json:"reason,omitempty"`
}

// RepFacts are the facts the release rules look at.
type RepFacts struct {
	State          State
	DueAt          time.Time
	EstJuniorHours float64
	StartedAt      time.Time // zero if not started
	StuckAskedAt   time.Time // zero if the junior has not been asked
	KeepGoingUntil time.Time // zero unless the junior chose to keep going
}

// CheckRelease applies the deadline and stuck rules in that order.
func CheckRelease(f RepFacts, p Policy, now time.Time) ReleaseCheck {
	if !f.State.Reserved() {
		return ReleaseCheck{}
	}
	elapsed := 0.0
	if !f.StartedAt.IsZero() {
		elapsed = now.Sub(f.StartedAt).Hours()
	}
	remaining := f.EstJuniorHours - elapsed
	if remaining < 0.25*f.EstJuniorHours {
		remaining = 0.25 * f.EstJuniorHours
	}
	if f.State != SeniorSignoff && f.DueAt.Sub(now).Hours() < remaining*p.SlackSafety {
		return ReleaseCheck{Release: true, Reason: "deadline at risk"}
	}
	if f.StartedAt.IsZero() || f.State != InProgress {
		return ReleaseCheck{}
	}
	if !f.KeepGoingUntil.IsZero() && now.Before(f.KeepGoingUntil) {
		return ReleaseCheck{}
	}
	if elapsed <= p.ReleaseStuckMultiplier*f.EstJuniorHours {
		return ReleaseCheck{}
	}
	// An ask is pending if it happened after the last "keep going" answer.
	pending := !f.StuckAskedAt.IsZero() && (f.KeepGoingUntil.IsZero() || f.StuckAskedAt.After(f.KeepGoingUntil))
	if !pending {
		return ReleaseCheck{AskFirst: true, Reason: "taking longer than twice the estimate"}
	}
	if now.Sub(f.StuckAskedAt).Minutes() >= p.StuckAnswerMinutes {
		return ReleaseCheck{Release: true, Reason: "stuck and no answer"}
	}
	return ReleaseCheck{}
}

// ProposalExpired reports whether a proposal waited too long for a senior.
func ProposalExpired(proposedAt time.Time, p Policy, now time.Time) bool {
	return !proposedAt.IsZero() && now.Sub(proposedAt).Hours() >= p.ProposalTimeoutHours
}

// HoldExpired reports whether a held ticket has waited long enough that the builder should take it.
func HoldExpired(triagedAt time.Time, p Policy, now time.Time) bool {
	return !triagedAt.IsZero() && now.Sub(triagedAt).Hours() >= p.HoldMaxHours
}
