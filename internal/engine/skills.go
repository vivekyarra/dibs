package engine

import "fmt"

// Level is how far a person has come with one skill.
type Level int

const (
	Unseen      Level = 0
	Exposed     Level = 1
	Guided      Level = 2
	Independent Level = 3
	Fluent      Level = 4
)

var levelNames = [...]string{"Unseen", "Exposed", "Guided", "Independent", "Fluent"}

func (l Level) String() string {
	if l < Unseen || l > Fluent {
		return fmt.Sprintf("Level(%d)", int(l))
	}
	return levelNames[l]
}

// SkillState is one person's standing in one skill.
type SkillState struct {
	Level           Level `json:"level"`
	CleanSinceLevel int   `json:"clean_since_level"`
}

// Weight says whether a skill is central to a ticket or incidental.
type Weight string

const (
	Primary   Weight = "primary"
	Secondary Weight = "secondary"
)

// SkillReq is one skill a ticket exercises.
type SkillReq struct {
	Name   string `json:"name"`
	Weight Weight `json:"weight"`
}

// SkillChange records one movement on the skill map, with its reason.
type SkillChange struct {
	Skill  string `json:"skill"`
	From   Level  `json:"from"`
	To     Level  `json:"to"`
	Reason string `json:"reason"`
}

// CleanThreshold is how many clean reps move Guided→Independent and Independent→Fluent.
const CleanThreshold = 2

// IsClean reports whether a merged rep counts as done without real help:
// at most a nudge, and at most one round of review changes.
func IsClean(maxHint int, reviewRounds int) bool {
	return maxHint <= 1 && reviewRounds <= 1
}

func primaries(reqs []SkillReq) []string {
	var out []string
	for _, r := range reqs {
		if r.Weight == Primary {
			out = append(out, r.Name)
		}
	}
	return out
}

// OnStart applies the "attempted it" rule: every primary skill that was
// Unseen becomes Exposed the moment the junior starts the rep.
func OnStart(levels map[string]SkillState, reqs []SkillReq) []SkillChange {
	var ch []SkillChange
	for _, s := range primaries(reqs) {
		st := levels[s]
		if st.Level == Unseen {
			levels[s] = SkillState{Level: Exposed}
			ch = append(ch, SkillChange{s, Unseen, Exposed, "started a rep"})
		}
	}
	return ch
}

// OnMerge applies the evidence of a merged rep to each primary skill.
func OnMerge(levels map[string]SkillState, reqs []SkillReq, maxHint, reviewRounds int) []SkillChange {
	clean := IsClean(maxHint, reviewRounds)
	var ch []SkillChange
	for _, s := range primaries(reqs) {
		st := levels[s]
		from := st.Level
		switch st.Level {
		case Unseen, Exposed:
			st = SkillState{Level: Guided}
		case Guided, Independent:
			if clean {
				st.CleanSinceLevel++
				if st.CleanSinceLevel >= CleanThreshold {
					st = SkillState{Level: st.Level + 1}
				}
			}
		case Fluent:
			// Fluent is the ceiling; nothing more to earn.
		}
		levels[s] = st
		if st.Level != from {
			reason := "merged a rep"
			if from >= Guided {
				reason = fmt.Sprintf("%d clean reps", CleanThreshold)
			}
			ch = append(ch, SkillChange{s, from, st.Level, reason})
		}
	}
	return ch
}

// OnRegression lowers each primary skill by one (never below Exposed) when a
// merged rep is reverted or linked to a regression.
func OnRegression(levels map[string]SkillState, reqs []SkillReq) []SkillChange {
	var ch []SkillChange
	for _, s := range primaries(reqs) {
		st := levels[s]
		if st.Level > Exposed {
			from := st.Level
			levels[s] = SkillState{Level: st.Level - 1}
			ch = append(ch, SkillChange{s, from, st.Level - 1, "regression"})
		}
	}
	return ch
}

// OnRelease is what a released rep still teaches: Unseen primaries become Exposed.
func OnRelease(levels map[string]SkillState, reqs []SkillReq) []SkillChange {
	return OnStart(levels, reqs)
}
