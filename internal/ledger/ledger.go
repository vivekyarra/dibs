// Package ledger is Dibs' source of truth: a SQLite database (WAL mode,
// immediate transactions) shared by every agent on the machine. All writes go
// through Tx so that three agents calling `dibs` at once never interleave.
package ledger

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vivekyarra/dibs/internal/engine"
	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" database/sql driver
)

//go:embed schema.sql
var schema string

// SchemaVersion is bumped when schema.sql changes incompatibly.
const SchemaVersion = "1"

// Ledger wraps the database.
type Ledger struct{ db *sql.DB }

// Open creates the database (and its folder) if needed and applies the schema.
func Open(path string) (*Ledger, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(15000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_txlock=immediate"
	if path == ":memory:" {
		dsn = "file::memory:?_txlock=immediate"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	}
	l := &Ledger{db: db}
	if err := l.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return l, nil
}

func (l *Ledger) migrate() error {
	return l.Tx(context.Background(), func(tx *Tx) error {
		if _, err := tx.tx.Exec(schema); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		var v string
		err := tx.tx.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&v)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err = tx.tx.Exec(`INSERT INTO meta(key,value) VALUES('schema_version',?)`, SchemaVersion)
			return err
		case err != nil:
			return err
		case v != SchemaVersion:
			return fmt.Errorf("ledger schema version %s, this dibs expects %s", v, SchemaVersion)
		}
		return nil
	})
}

// Close closes the database.
func (l *Ledger) Close() error { return l.db.Close() }

// Tx runs fn in an immediate transaction, retrying briefly if another agent holds the lock.
func (l *Ledger) Tx(ctx context.Context, fn func(*Tx) error) error {
	var last error
	for attempt := 0; attempt < 8; attempt++ {
		sqlTx, err := l.db.BeginTx(ctx, nil)
		if err != nil {
			if isBusy(err) {
				last = err
				time.Sleep(time.Duration(25*(attempt+1)) * time.Millisecond)
				continue
			}
			return err
		}
		if err := fn(&Tx{tx: sqlTx}); err != nil {
			sqlTx.Rollback()
			if isBusy(err) {
				last = err
				time.Sleep(time.Duration(25*(attempt+1)) * time.Millisecond)
				continue
			}
			return err
		}
		if err := sqlTx.Commit(); err != nil {
			if isBusy(err) {
				last = err
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("ledger busy: %w", last)
}

func isBusy(err error) bool {
	s := err.Error()
	return strings.Contains(s, "SQLITE_BUSY") || strings.Contains(s, "database is locked")
}

// Tx is an open transaction.
type Tx struct{ tx *sql.Tx }

const tsLayout = time.RFC3339Nano

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(tsLayout)
}

func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(tsLayout, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Ticket is one GitHub issue as Dibs tracks it.
type Ticket struct {
	Number         int               `json:"number"`
	Title          string            `json:"title"`
	URL            string            `json:"url"`
	Body           string            `json:"-"`
	State          engine.State      `json:"state"`
	ReservedFor    string            `json:"reserved_for,omitempty"`
	Skills         []engine.SkillReq `json:"skills"`
	TouchedPaths   []string          `json:"touched_paths"`
	EstSeniorHours float64           `json:"est_senior_hours"`
	EstJuniorHours float64           `json:"est_junior_hours"`
	DueAt          time.Time         `json:"due_at"`
	Score          float64           `json:"score"`
	Breakdown      engine.Breakdown  `json:"breakdown"`
	Reason         string            `json:"reason,omitempty"`
	TriagedAt      time.Time         `json:"triaged_at"`
	ProposedAt     time.Time         `json:"proposed_at"`
	StartedAt      time.Time         `json:"started_at"`
	StuckAskedAt   time.Time         `json:"stuck_asked_at"`
	KeepGoingUntil time.Time         `json:"keep_going_until"`
	PRNumber       int               `json:"pr_number,omitempty"`
	PRURL          string            `json:"pr_url,omitempty"`
	PRAuthor       string            `json:"pr_author,omitempty"`
	Branch         string            `json:"branch,omitempty"`
	MaxHintLevel   int               `json:"max_hint_level"`
	ReviewRounds   int               `json:"review_rounds"`
	Attempts       int               `json:"attempts"`
	SeniorUnlocked int               `json:"senior_unlocked"`
	Approvals      int               `json:"approvals"`
	SeniorComments int               `json:"senior_comments"`
	Teaching       int               `json:"teaching_moments"`
	WasReleased    bool              `json:"was_released"`
	ReleasedReason string            `json:"released_reason,omitempty"`
	ReleasedFor    string            `json:"released_for,omitempty"`
	MergedAt       time.Time         `json:"merged_at"`
	Sample         bool              `json:"sample,omitempty"`
}

const ticketCols = `number,title,url,body,state,reserved_for,skills,touched_paths,est_senior_hours,est_junior_hours,
due_at,score,score_breakdown,reason,triaged_at,proposed_at,started_at,stuck_asked_at,keep_going_until,pr_number,pr_url,
pr_author,branch,max_hint_level,review_rounds,attempts,senior_unlocked,approvals,senior_comments,teaching,was_released,
released_reason,released_for,merged_at,sample`

type scanner interface{ Scan(...any) error }

func scanTicket(r scanner) (Ticket, error) {
	var t Ticket
	var state, skills, paths, bd, due, tri, prop, start, stuck, keep, merged string
	var released, sample int
	err := r.Scan(&t.Number, &t.Title, &t.URL, &t.Body, &state, &t.ReservedFor, &skills, &paths, &t.EstSeniorHours,
		&t.EstJuniorHours, &due, &t.Score, &bd, &t.Reason, &tri, &prop, &start, &stuck, &keep, &t.PRNumber, &t.PRURL,
		&t.PRAuthor, &t.Branch, &t.MaxHintLevel, &t.ReviewRounds, &t.Attempts, &t.SeniorUnlocked, &t.Approvals,
		&t.SeniorComments, &t.Teaching, &released, &t.ReleasedReason, &t.ReleasedFor, &merged, &sample)
	if err != nil {
		return t, err
	}
	t.State = engine.State(state)
	if err := json.Unmarshal([]byte(skills), &t.Skills); err != nil {
		return t, fmt.Errorf("ticket #%d skills: %w", t.Number, err)
	}
	if err := json.Unmarshal([]byte(paths), &t.TouchedPaths); err != nil {
		return t, fmt.Errorf("ticket #%d paths: %w", t.Number, err)
	}
	_ = json.Unmarshal([]byte(bd), &t.Breakdown)
	t.DueAt, t.TriagedAt, t.ProposedAt, t.StartedAt = parseTS(due), parseTS(tri), parseTS(prop), parseTS(start)
	t.StuckAskedAt, t.KeepGoingUntil, t.MergedAt = parseTS(stuck), parseTS(keep), parseTS(merged)
	t.WasReleased, t.Sample = released == 1, sample == 1
	if t.Skills == nil {
		t.Skills = []engine.SkillReq{}
	}
	if t.TouchedPaths == nil {
		t.TouchedPaths = []string{}
	}
	return t, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// SaveTicket inserts or fully replaces a ticket row.
func (tx *Tx) SaveTicket(t Ticket, now time.Time) error {
	if !t.State.Valid() {
		return fmt.Errorf("ticket #%d: invalid state %q", t.Number, t.State)
	}
	if t.Skills == nil {
		t.Skills = []engine.SkillReq{}
	}
	if t.TouchedPaths == nil {
		t.TouchedPaths = []string{}
	}
	skills, _ := json.Marshal(t.Skills)
	paths, _ := json.Marshal(t.TouchedPaths)
	bd, _ := json.Marshal(t.Breakdown)
	_, err := tx.tx.Exec(`INSERT OR REPLACE INTO tickets(`+ticketCols+`,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.Number, t.Title, t.URL, t.Body, string(t.State), t.ReservedFor, string(skills), string(paths), t.EstSeniorHours,
		t.EstJuniorHours, ts(t.DueAt), t.Score, string(bd), t.Reason, ts(t.TriagedAt), ts(t.ProposedAt), ts(t.StartedAt),
		ts(t.StuckAskedAt), ts(t.KeepGoingUntil), t.PRNumber, t.PRURL, t.PRAuthor, t.Branch, t.MaxHintLevel, t.ReviewRounds,
		t.Attempts, t.SeniorUnlocked, t.Approvals, t.SeniorComments, t.Teaching, b2i(t.WasReleased), t.ReleasedReason,
		t.ReleasedFor, ts(t.MergedAt), b2i(t.Sample), ts(now))
	return err
}

// ErrNoTicket is returned when a ticket number is unknown to the ledger.
var ErrNoTicket = errors.New("unknown ticket")

// Ticket loads one ticket.
func (tx *Tx) Ticket(n int) (Ticket, error) {
	t, err := scanTicket(tx.tx.QueryRow(`SELECT `+ticketCols+` FROM tickets WHERE number=?`, n))
	if errors.Is(err, sql.ErrNoRows) {
		return t, fmt.Errorf("#%d: %w (run `dibs sync` first)", n, ErrNoTicket)
	}
	return t, err
}

// Tickets loads tickets in the given states (all tickets if none given), by number.
func (tx *Tx) Tickets(states ...engine.State) ([]Ticket, error) {
	q := `SELECT ` + ticketCols + ` FROM tickets`
	var args []any
	if len(states) > 0 {
		q += ` WHERE state IN (` + strings.TrimSuffix(strings.Repeat("?,", len(states)), ",") + `)`
		for _, s := range states {
			args = append(args, string(s))
		}
	}
	rows, err := tx.tx.Query(q+` ORDER BY number`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ticket
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Transition moves a ticket to a new state if the lifecycle allows it and logs the move.
func (tx *Tx) Transition(t *Ticket, to engine.State, actor string, now time.Time, data map[string]any) error {
	if t.State != to && !engine.CanTransition(t.State, to) {
		return &engine.TransitionError{Ticket: t.Number, From: t.State, To: to}
	}
	from := t.State
	t.State = to
	if err := tx.SaveTicket(*t, now); err != nil {
		return err
	}
	if data == nil {
		data = map[string]any{}
	}
	data["from"], data["to"] = from, to
	return tx.Event(t.Number, actor, "transition", now, data)
}

// Event appends to the event log.
func (tx *Tx) Event(ticket int, actor, kind string, now time.Time, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = tx.tx.Exec(`INSERT INTO events(at,ticket,actor,kind,data) VALUES(?,?,?,?,?)`, ts(now), ticket, actor, kind, string(b))
	return err
}

// EventRow is one entry of the event log.
type EventRow struct {
	ID     int64           `json:"id"`
	At     time.Time       `json:"at"`
	Ticket int             `json:"ticket"`
	Actor  string          `json:"actor"`
	Kind   string          `json:"kind"`
	Data   json.RawMessage `json:"data"`
}

// Events returns events for a ticket (0 = all tickets) of the given kinds (none = all), oldest first.
func (tx *Tx) Events(ticket int, kinds ...string) ([]EventRow, error) {
	q := `SELECT id,at,ticket,actor,kind,data FROM events WHERE 1=1`
	var args []any
	if ticket != 0 {
		q += ` AND ticket=?`
		args = append(args, ticket)
	}
	if len(kinds) > 0 {
		q += ` AND kind IN (` + strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",") + `)`
		for _, k := range kinds {
			args = append(args, k)
		}
	}
	rows, err := tx.tx.Query(q+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var e EventRow
		var at, data string
		if err := rows.Scan(&e.ID, &at, &e.Ticket, &e.Actor, &e.Kind, &data); err != nil {
			return nil, err
		}
		e.At, e.Data = parseTS(at), json.RawMessage(data)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Levels returns a person's skill map.
func (tx *Tx) Levels(handle string) (map[string]engine.SkillState, error) {
	rows, err := tx.tx.Query(`SELECT skill,level,clean_since_level FROM skill_levels WHERE handle=?`, handle)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]engine.SkillState{}
	for rows.Next() {
		var s string
		var st engine.SkillState
		if err := rows.Scan(&s, &st.Level, &st.CleanSinceLevel); err != nil {
			return nil, err
		}
		out[s] = st
	}
	return out, rows.Err()
}

// SetLevels writes a person's skill map.
func (tx *Tx) SetLevels(handle string, levels map[string]engine.SkillState, now time.Time) error {
	for s, st := range levels {
		if _, err := tx.tx.Exec(`INSERT INTO skill_levels(handle,skill,level,clean_since_level,updated_at) VALUES(?,?,?,?,?)
ON CONFLICT(handle,skill) DO UPDATE SET level=excluded.level, clean_since_level=excluded.clean_since_level, updated_at=excluded.updated_at`,
			handle, s, int(st.Level), st.CleanSinceLevel, ts(now)); err != nil {
			return err
		}
	}
	return nil
}

// HasLevels reports whether a person already has any skill rows (so init doesn't overwrite earned levels).
func (tx *Tx) HasLevels(handle string) (bool, error) {
	var n int
	err := tx.tx.QueryRow(`SELECT COUNT(*) FROM skill_levels WHERE handle=?`, handle).Scan(&n)
	return n > 0, err
}

// Meta reads a meta value ("" if missing).
func (tx *Tx) Meta(key string) (string, error) {
	var v string
	err := tx.tx.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetMeta writes a meta value.
func (tx *Tx) SetMeta(key, value string) error {
	_, err := tx.tx.Exec(`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// Reset deletes all tickets, events and levels (used by `dibs reset` for demo takes).
func (tx *Tx) Reset() error {
	for _, q := range []string{`DELETE FROM tickets`, `DELETE FROM events`, `DELETE FROM skill_levels`, `DELETE FROM meta WHERE key<>'schema_version'`} {
		if _, err := tx.tx.Exec(q); err != nil {
			return err
		}
	}
	return nil
}
