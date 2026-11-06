// Package engine holds every deterministic rule Dibs applies. Nothing in this
// package performs I/O: it takes facts and returns decisions, so each rule can
// be tested exhaustively and explained to a human ("the model proposes, the
// ledger decides").
package engine

import "fmt"

// State is a ticket's position in the Dibs lifecycle.
type State string

const (
	New           State = "NEW"
	Triaged       State = "TRIAGED"
	Proposed      State = "PROPOSED"
	Reserved      State = "RESERVED"
	InProgress    State = "IN_PROGRESS"
	JuniorPROpen  State = "JUNIOR_PR_OPEN"
	SeniorSignoff State = "SENIOR_SIGNOFF"
	Merged        State = "MERGED"
	Recorded      State = "EVIDENCE_RECORDED"
	Released      State = "RELEASED"
	ToBuilder     State = "TO_BUILDER"
	BuilderPR     State = "BUILDER_PR"
	Closed        State = "CLOSED"
)

// AllStates lists every state in lifecycle order.
var AllStates = []State{New, Triaged, Proposed, Reserved, InProgress, JuniorPROpen,
	SeniorSignoff, Merged, Recorded, Released, ToBuilder, BuilderPR, Closed}

var transitions = map[State][]State{
	New:           {Triaged, Closed},
	Triaged:       {Triaged, Proposed, ToBuilder, Closed},
	Proposed:      {Reserved, ToBuilder, Triaged, Closed},
	Reserved:      {InProgress, JuniorPROpen, Released, Closed},
	InProgress:    {JuniorPROpen, Released, Closed},
	JuniorPROpen:  {SeniorSignoff, Merged, InProgress, Released, Closed},
	SeniorSignoff: {Merged, JuniorPROpen, Released, Closed},
	Merged:        {Recorded},
	Released:      {ToBuilder},
	ToBuilder:     {BuilderPR, Merged, Closed},
	BuilderPR:     {Merged, ToBuilder, Closed},
	Recorded:      {},
	Closed:        {},
}

// CanTransition reports whether moving from one state to another is allowed.
func CanTransition(from, to State) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Valid reports whether s is a known state.
func (s State) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// Reserved reports whether the ticket currently belongs to a junior, which
// is when the hint ladder and the guard apply.
func (s State) Reserved() bool {
	switch s {
	case Reserved, InProgress, JuniorPROpen, SeniorSignoff:
		return true
	}
	return false
}

// Open reports whether the ticket still counts toward the open backlog.
func (s State) Open() bool {
	switch s {
	case Merged, Recorded, Closed:
		return false
	}
	return true
}

// TransitionError is returned when a caller asks for a move the lifecycle forbids.
type TransitionError struct {
	Ticket   int
	From, To State
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("ticket #%d cannot move from %s to %s", e.Ticket, e.From, e.To)
}
