package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vivekyarra/dibs/internal/engine"
	"github.com/vivekyarra/dibs/internal/ledger"
)

var now = time.Date(2026, 11, 6, 12, 0, 0, 123456789, time.UTC)

func openTemp(t *testing.T) (*ledger.Ledger, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sub", "dir", "dibs.sqlite") // Open must create folders
	l, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, path
}

func tx(t *testing.T, l *ledger.Ledger, fn func(*ledger.Tx) error) {
	t.Helper()
	if err := l.Tx(context.Background(), fn); err != nil {
		t.Fatal(err)
	}
}

// fullTicket sets every field to a non-zero value, with times in a non-UTC zone.
func fullTicket(n int) ledger.Ticket {
	ist := time.FixedZone("IST", 5*3600+1800)
	base := time.Date(2026, 11, 3, 10, 30, 15, 987654321, ist)
	return ledger.Ticket{
		Number: n, Title: "Make POST /orders idempotent ✓", URL: "https://github.com/o/r/issues/3",
		Body: "line1\nline2 'quoted' \"dq\"", State: engine.Reserved, ReservedFor: "asha",
		Skills:         []engine.SkillReq{{Name: "idempotency", Weight: engine.Primary}, {Name: "http-handlers", Weight: engine.Secondary}},
		TouchedPaths:   []string{"app/orders/service.py", "app/api/orders.py"},
		EstSeniorHours: 0.5, EstJuniorHours: 1.25, DueAt: base.Add(72 * time.Hour), Score: 6.83,
		Breakdown: engine.Breakdown{Growth: 6, SlackBonus: 1, UnseenPenalty: 0.5, Score: 6.5, EstJuniorHours: 1.25,
			SlackDays: 2.9, Levels: map[string]engine.Level{"idempotency": 1}, GrowthBySkill: map[string]float64{"idempotency": 3},
			RiskyPath: "x", BacklogShareNew: 0.1},
		Reason: "proposed", TriagedAt: base, ProposedAt: base.Add(time.Minute), StartedAt: base.Add(2 * time.Minute),
		StuckAskedAt: base.Add(3 * time.Minute), KeepGoingUntil: base.Add(4 * time.Minute), PRNumber: 11,
		PRURL: "https://github.com/o/r/pull/11", PRAuthor: "asha-gh", Branch: "asha/idem", MaxHintLevel: 2,
		ReviewRounds: 1, Attempts: 3, SeniorUnlocked: 4, Approvals: 2, SeniorComments: 5, Teaching: 1,
		WasReleased: true, ReleasedReason: "deadline at risk", ReleasedFor: "builder", MergedAt: base.Add(5 * time.Minute),
		Sample: true,
	}
}

// normTimes converts every time field to UTC so reflect.DeepEqual compares instants.
func normTimes(t ledger.Ticket) ledger.Ticket {
	for _, p := range []*time.Time{&t.DueAt, &t.TriagedAt, &t.ProposedAt, &t.StartedAt, &t.StuckAskedAt, &t.KeepGoingUntil, &t.MergedAt} {
		if !p.IsZero() {
			*p = p.UTC()
		}
	}
	return t
}

func TestOpenSchemaVersion(t *testing.T) {
	l, _ := openTemp(t)
	tx(t, l, func(tx *ledger.Tx) error {
		v, err := tx.Meta("schema_version")
		if err != nil || v != ledger.SchemaVersion {
			t.Fatalf("schema_version %q %v", v, err)
		}
		if v, err := tx.Meta("missing"); err != nil || v != "" {
			t.Fatalf("missing meta %q %v", v, err)
		}
		return nil
	})
}

func TestTicketRoundTripEveryField(t *testing.T) {
	l, _ := openTemp(t)
	in := fullTicket(3)
	tx(t, l, func(tx *ledger.Tx) error { return tx.SaveTicket(in, now) })
	var got ledger.Ticket
	tx(t, l, func(tx *ledger.Tx) (err error) { got, err = tx.Ticket(3); return })
	if !reflect.DeepEqual(got, normTimes(in)) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, normTimes(in))
	}
	for _, tm := range []time.Time{got.DueAt, got.TriagedAt, got.MergedAt} {
		if tm.Location() != time.UTC {
			t.Fatalf("time not UTC: %v", tm.Location())
		}
	}
	// Reflect guard: if someone adds a field to Ticket, make sure fullTicket sets it
	// (so this test keeps covering EVERY column).
	v := reflect.ValueOf(in)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			t.Errorf("fullTicket leaves %s zero; extend it", v.Type().Field(i).Name)
		}
	}
}

func TestTicketZeroValuesRoundTrip(t *testing.T) {
	l, _ := openTemp(t)
	in := ledger.Ticket{Number: 9, Title: "t", State: engine.New}
	tx(t, l, func(tx *ledger.Tx) error { return tx.SaveTicket(in, now) })
	tx(t, l, func(tx *ledger.Tx) error {
		got, err := tx.Ticket(9)
		if err != nil {
			return err
		}
		if got.Skills == nil || got.TouchedPaths == nil || len(got.Skills) != 0 {
			t.Fatalf("nil slices should load as empty: %+v", got)
		}
		if !got.DueAt.IsZero() || !got.MergedAt.IsZero() {
			t.Fatal("zero times must stay zero")
		}
		return nil
	})
}

func TestSaveTicketRejectsInvalidState(t *testing.T) {
	l, _ := openTemp(t)
	err := l.Tx(context.Background(), func(tx *ledger.Tx) error {
		return tx.SaveTicket(ledger.Ticket{Number: 1, State: "BOGUS"}, now)
	})
	if err == nil || !strings.Contains(err.Error(), "invalid state") {
		t.Fatalf("got %v", err)
	}
}

func TestUnknownTicket(t *testing.T) {
	l, _ := openTemp(t)
	err := l.Tx(context.Background(), func(tx *ledger.Tx) error { _, err := tx.Ticket(42); return err })
	if !errors.Is(err, ledger.ErrNoTicket) {
		t.Fatalf("want ErrNoTicket, got %v", err)
	}
}

func TestTicketsFilterAndOrder(t *testing.T) {
	l, _ := openTemp(t)
	states := map[int]engine.State{7: engine.Triaged, 2: engine.Reserved, 5: engine.Triaged, 1: engine.Closed, 9: engine.InProgress}
	tx(t, l, func(tx *ledger.Tx) error {
		for n, s := range states {
			if err := tx.SaveTicket(ledger.Ticket{Number: n, Title: "t", State: s}, now); err != nil {
				return err
			}
		}
		return nil
	})
	nums := func(ts []ledger.Ticket) []int {
		var out []int
		for _, t := range ts {
			out = append(out, t.Number)
		}
		return out
	}
	tx(t, l, func(tx *ledger.Tx) error {
		all, err := tx.Tickets()
		if err != nil {
			return err
		}
		if got := nums(all); !reflect.DeepEqual(got, []int{1, 2, 5, 7, 9}) {
			t.Fatalf("all: %v", got)
		}
		tri, _ := tx.Tickets(engine.Triaged)
		if got := nums(tri); !reflect.DeepEqual(got, []int{5, 7}) {
			t.Fatalf("triaged: %v", got)
		}
		res, _ := tx.Tickets(engine.InProgress, engine.Reserved)
		if got := nums(res); !reflect.DeepEqual(got, []int{2, 9}) {
			t.Fatalf("reserved: %v", got)
		}
		none, err := tx.Tickets(engine.Merged)
		if err != nil || len(none) != 0 {
			t.Fatalf("merged: %v %v", none, err)
		}
		return nil
	})
}

func TestSaveTicketReplaces(t *testing.T) {
	l, _ := openTemp(t)
	tx(t, l, func(tx *ledger.Tx) error {
		if err := tx.SaveTicket(fullTicket(3), now); err != nil {
			return err
		}
		return tx.SaveTicket(ledger.Ticket{Number: 3, Title: "new", State: engine.Triaged}, now)
	})
	tx(t, l, func(tx *ledger.Tx) error {
		got, err := tx.Ticket(3)
		if err != nil {
			return err
		}
		if got.Title != "new" || got.Attempts != 0 || got.ReservedFor != "" || len(got.Skills) != 0 {
			t.Fatalf("not fully replaced: %+v", got)
		}
		return nil
	})
}

func TestTransitionLegalAndIllegal(t *testing.T) {
	l, _ := openTemp(t)
	tk := ledger.Ticket{Number: 3, Title: "t", State: engine.New}
	tx(t, l, func(tx *ledger.Tx) error { return tx.SaveTicket(tk, now) })
	path := []engine.State{engine.Triaged, engine.Proposed, engine.Reserved, engine.InProgress, engine.JuniorPROpen,
		engine.SeniorSignoff, engine.Merged, engine.Recorded}
	for i, to := range path {
		tx(t, l, func(tx *ledger.Tx) error {
			return tx.Transition(&tk, to, "dibs", now.Add(time.Duration(i)*time.Second), map[string]any{"step": i})
		})
		if tk.State != to {
			t.Fatalf("in-memory state %s, want %s", tk.State, to)
		}
	}
	// Illegal: Recorded is terminal.
	err := l.Tx(context.Background(), func(tx *ledger.Tx) error {
		return tx.Transition(&tk, engine.Reserved, "dibs", now, nil)
	})
	var te *engine.TransitionError
	if !errors.As(err, &te) || te.From != engine.Recorded || te.To != engine.Reserved || te.Ticket != 3 {
		t.Fatalf("want TransitionError, got %v", err)
	}
	if tk.State != engine.Recorded {
		t.Fatal("failed transition must not change the ticket")
	}
	// Same-state transition is a no-op move that is still logged.
	tx(t, l, func(tx *ledger.Tx) error { return tx.Transition(&tk, engine.Recorded, "dibs", now, nil) })

	tx(t, l, func(tx *ledger.Tx) error {
		got, _ := tx.Ticket(3)
		if got.State != engine.Recorded {
			t.Fatalf("stored state %s", got.State)
		}
		evs, err := tx.Events(3, "transition")
		if err != nil {
			return err
		}
		if len(evs) != len(path)+1 {
			t.Fatalf("got %d transition events", len(evs))
		}
		var d map[string]any
		if err := json.Unmarshal(evs[0].Data, &d); err != nil {
			return err
		}
		if d["from"] != "NEW" || d["to"] != "TRIAGED" || d["step"] != float64(0) {
			t.Fatalf("event data %v", d)
		}
		if evs[0].Actor != "dibs" || !evs[0].At.Equal(now) || evs[1].ID <= evs[0].ID {
			t.Fatalf("event row %+v", evs[0])
		}
		return nil
	})
}

func TestTransitionRolledBackWithTx(t *testing.T) {
	l, _ := openTemp(t)
	tk := ledger.Ticket{Number: 1, Title: "t", State: engine.New}
	tx(t, l, func(tx *ledger.Tx) error { return tx.SaveTicket(tk, now) })
	boom := errors.New("boom")
	err := l.Tx(context.Background(), func(tx *ledger.Tx) error {
		if err := tx.Transition(&tk, engine.Triaged, "dibs", now, nil); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	tx(t, l, func(tx *ledger.Tx) error {
		got, _ := tx.Ticket(1)
		evs, _ := tx.Events(0)
		if got.State != engine.New || len(evs) != 0 {
			t.Fatalf("rollback failed: %s, %d events", got.State, len(evs))
		}
		return nil
	})
}

func TestEventsFilter(t *testing.T) {
	l, _ := openTemp(t)
	tx(t, l, func(tx *ledger.Tx) error {
		for i, e := range []struct {
			n    int
			kind string
		}{{1, "hint_given"}, {2, "hint_given"}, {1, "attempt"}, {0, "sweep"}, {1, "hint_given"}} {
			if err := tx.Event(e.n, "a", e.kind, now.Add(time.Duration(i)*time.Second), map[string]int{"i": i}); err != nil {
				return err
			}
		}
		return nil
	})
	count := func(ticket int, kinds ...string) int {
		var n int
		tx(t, l, func(tx *ledger.Tx) error {
			evs, err := tx.Events(ticket, kinds...)
			for i := 1; i < len(evs); i++ {
				if evs[i].ID <= evs[i-1].ID {
					t.Fatal("events not oldest first")
				}
			}
			n = len(evs)
			return err
		})
		return n
	}
	cases := []struct {
		ticket int
		kinds  []string
		want   int
	}{{0, nil, 5}, {1, nil, 3}, {1, []string{"hint_given"}, 2}, {0, []string{"hint_given"}, 3},
		{0, []string{"hint_given", "sweep"}, 4}, {2, []string{"attempt"}, 0}, {99, nil, 0}}
	for _, c := range cases {
		if got := count(c.ticket, c.kinds...); got != c.want {
			t.Errorf("Events(%d, %v) = %d, want %d", c.ticket, c.kinds, got, c.want)
		}
	}
	// Unmarshalable data is an error, not a panic.
	err := l.Tx(context.Background(), func(tx *ledger.Tx) error { return tx.Event(1, "a", "x", now, make(chan int)) })
	if err == nil {
		t.Fatal("want marshal error")
	}
}

func TestLevelsUpsert(t *testing.T) {
	l, _ := openTemp(t)
	tx(t, l, func(tx *ledger.Tx) error {
		has, err := tx.HasLevels("asha")
		if err != nil || has {
			t.Fatalf("HasLevels on empty: %v %v", has, err)
		}
		if m, err := tx.Levels("asha"); err != nil || len(m) != 0 || m == nil {
			t.Fatalf("empty levels %v %v", m, err)
		}
		if err := tx.SetLevels("asha", map[string]engine.SkillState{"idem": {Level: 1}, "http": {Level: 3, CleanSinceLevel: 1}}, now); err != nil {
			return err
		}
		if err := tx.SetLevels("asha", map[string]engine.SkillState{"idem": {Level: 2, CleanSinceLevel: 1}}, now); err != nil {
			return err
		}
		if err := tx.SetLevels("ben", map[string]engine.SkillState{"idem": {Level: 4}}, now); err != nil {
			return err
		}
		return nil
	})
	tx(t, l, func(tx *ledger.Tx) error {
		m, err := tx.Levels("asha")
		if err != nil {
			return err
		}
		want := map[string]engine.SkillState{"idem": {Level: 2, CleanSinceLevel: 1}, "http": {Level: 3, CleanSinceLevel: 1}}
		if !reflect.DeepEqual(m, want) {
			t.Fatalf("levels %v", m)
		}
		if has, _ := tx.HasLevels("asha"); !has {
			t.Fatal("HasLevels")
		}
		return nil
	})
	// The schema CHECK constraint refuses impossible levels.
	err := l.Tx(context.Background(), func(tx *ledger.Tx) error {
		return tx.SetLevels("asha", map[string]engine.SkillState{"x": {Level: 7}}, now)
	})
	if err == nil {
		t.Fatal("level 7 accepted")
	}
}

func TestMetaAndReset(t *testing.T) {
	l, _ := openTemp(t)
	tx(t, l, func(tx *ledger.Tx) error {
		if err := tx.SetMeta("last_sweep", "a"); err != nil {
			return err
		}
		if err := tx.SetMeta("last_sweep", "b"); err != nil {
			return err
		}
		if v, _ := tx.Meta("last_sweep"); v != "b" {
			t.Fatalf("meta %q", v)
		}
		tx.SaveTicket(fullTicket(1), now)
		tx.Event(1, "a", "k", now, nil)
		tx.SetLevels("asha", map[string]engine.SkillState{"a": {Level: 1}}, now)
		return tx.Reset()
	})
	tx(t, l, func(tx *ledger.Tx) error {
		ts, _ := tx.Tickets()
		evs, _ := tx.Events(0)
		has, _ := tx.HasLevels("asha")
		m, _ := tx.Meta("last_sweep")
		v, _ := tx.Meta("schema_version")
		if len(ts) != 0 || len(evs) != 0 || has || m != "" || v != ledger.SchemaVersion {
			t.Fatalf("reset left %d tickets %d events levels=%v meta=%q version=%q", len(ts), len(evs), has, m, v)
		}
		return nil
	})
}

func TestReopenKeepsData(t *testing.T) {
	l, path := openTemp(t)
	tx(t, l, func(tx *ledger.Tx) error {
		if err := tx.SaveTicket(fullTicket(3), now); err != nil {
			return err
		}
		return tx.SetLevels("asha", map[string]engine.SkillState{"idem": {Level: 2}}, now)
	})
	l.Close()
	l2, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	tx(t, l2, func(tx *ledger.Tx) error {
		got, err := tx.Ticket(3)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got, normTimes(fullTicket(3))) {
			t.Fatalf("after reopen: %+v", got)
		}
		m, _ := tx.Levels("asha")
		if m["idem"].Level != 2 {
			t.Fatal("levels lost")
		}
		return nil
	})
}

func TestSchemaVersionMismatch(t *testing.T) {
	l, path := openTemp(t)
	tx(t, l, func(tx *ledger.Tx) error { return tx.SetMeta("schema_version", "99") })
	l.Close()
	l2, err := ledger.Open(path)
	if err == nil {
		l2.Close()
		t.Fatal("opened a ledger with a foreign schema version")
	}
	if !strings.Contains(err.Error(), "schema version 99") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestOpenMemory(t *testing.T) {
	l, err := ledger.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	tx(t, l, func(tx *ledger.Tx) error { return tx.SaveTicket(fullTicket(1), now) })
	tx(t, l, func(tx *ledger.Tx) error { _, err := tx.Ticket(1); return err })
}

func TestTxPropagatesErrorAndContext(t *testing.T) {
	l, _ := openTemp(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.Tx(ctx, func(*ledger.Tx) error { return nil }); err == nil {
		t.Fatal("cancelled context should fail")
	}
}

// Regression guard: a non-finite float (NaN/Inf from a bad estimate) must never
// leave a row that Tickets() can no longer scan (every sweep calls it).
// Verified with modernc sqlite: SaveTicket accepts it and reads still succeed.
func TestNonFiniteFloatDoesNotPoisonLedger(t *testing.T) {
	l, _ := openTemp(t)
	bad := ledger.Ticket{Number: 1, Title: "t", State: engine.Triaged, EstSeniorHours: math.NaN(), Score: math.Inf(1)}
	saveErr := l.Tx(context.Background(), func(tx *ledger.Tx) error { return tx.SaveTicket(bad, now) })
	readErr := l.Tx(context.Background(), func(tx *ledger.Tx) error { _, err := tx.Tickets(); return err })
	t.Logf("save err: %v; read err: %v", saveErr, readErr)
	if saveErr == nil && readErr != nil {
		t.Fatalf("SaveTicket accepted a NaN/Inf ticket that can no longer be read: %v", readErr)
	}
}
