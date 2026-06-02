package emulator

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"
)

// A scenario is the test-bed's PRIMARY driver (PLAN §5 Phase 7): a scripted,
// deterministic timeline of timed actions that mutate the fault injectors
// (price shift, latency) on cue, so an OMS / strategy under test faces regime
// changes — a manufactured dislocation that opens then closes, a feed that
// slows then recovers — at reproducible offsets from the run's start.
//
// Format: JSONL (one JSON object per line). Lines that are blank or begin with
// '#' (after leading whitespace) are skipped, so a scenario can be commented.
// Each object is an Action. Comments must be on their own line (a leading
// '#'); trailing data after an object on the same line is rejected.
//
//	# open a +15bp cross-venue dislocation
//	{"at_ms": 0,     "action": "price_shift", "params": {"offset_bps": 15, "scale": 1.0}}
//	{"at_ms": 5000,  "action": "latency",     "params": {"feed_to_book_ms": 50, "jitter_ms": 10}}
//	# close the dislocation
//	{"at_ms": 10000, "action": "price_shift", "params": {"offset_bps": 0}}
//
// The schedule is a pure function of (start, at_ms, speed): event i fires at
// start + at_ms_i/speed, so a run is reproducible bit-for-bit given the same
// start. applyDue lets tests step the timeline against a synthetic clock with
// no real sleeps.

// Action kinds.
const (
	actionPriceShift = "price_shift"
	actionLatency    = "latency"

	// maxAtMs caps an event offset at ~1 year, well below the int64-nanosecond
	// overflow of time.Duration(at_ms)*time.Millisecond.
	maxAtMs = int64(365 * 24 * 60 * 60 * 1000)
)

// Action is one timed scenario event. AtMs is the offset from the scenario
// start (>= 0). Action is the kind; Params is the raw JSON payload, decoded
// per-kind at parse time into the typed fields below (kept on the struct so an
// applied event needs no re-decode).
type Action struct {
	AtMs   int64  `json:"at_ms"`
	Action string `json:"action"`

	// Decoded payload (populated by ParseScenario). Only the fields relevant to
	// Action are meaningful.
	priceShift PriceShift
	latency    LatencyConfig
}

// priceShiftParams is the price_shift payload. Scale is a pointer so an omitted
// scale stays 0 (→ NewPriceShift normalizes to 1 = no multiplicative shift)
// rather than being indistinguishable from an explicit 0.
type priceShiftParams struct {
	OffsetBps float64  `json:"offset_bps"`
	Scale     *float64 `json:"scale"`
}

// latencyParams is the latency payload. All fields are milliseconds; omitted
// fields default to 0 (no delay on that edge).
type latencyParams struct {
	FeedToBookMs int `json:"feed_to_book_ms"`
	OrderAckMs   int `json:"order_ack_ms"`
	FillReportMs int `json:"fill_report_ms"`
	JitterMs     int `json:"jitter_ms"`
}

// Scenario is a parsed, validated, time-sorted timeline plus the runner's
// firing cursor (fired tracks how many events applyDue has already applied).
type Scenario struct {
	// Seed seeds the jitter RNG of any Latency this scenario swaps in, so a
	// latency action is as reproducible as the static-config path.
	Seed int64

	events []Action
	fired  int
}

// Len reports the number of events in the scenario.
func (sc *Scenario) Len() int { return len(sc.events) }

// ParseScenario reads a JSONL scenario from r, validates and decodes each
// action, and returns a Scenario with events stably sorted by AtMs. Errors
// carry the source line number. seed seeds any Latency the scenario swaps in.
func ParseScenario(r io.Reader, seed int64) (*Scenario, error) {
	sc := &Scenario{Seed: seed}
	scanner := bufio.NewScanner(r)
	// Allow long lines (default 64KiB token limit is plenty, but be generous).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue // blank or comment line
		}
		a, err := parseAction([]byte(text))
		if err != nil {
			return nil, fmt.Errorf("scenario line %d: %w", line, err)
		}
		sc.events = append(sc.events, a)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scenario read: %w", err)
	}

	// Stable sort by AtMs so events authored at the same offset fire in source
	// order — deterministic and intuitive for hand-authored timelines.
	sort.SliceStable(sc.events, func(i, j int) bool {
		return sc.events[i].AtMs < sc.events[j].AtMs
	})
	return sc, nil
}

// parseAction decodes and validates one JSONL object.
func parseAction(raw []byte) (Action, error) {
	var env struct {
		AtMs   int64           `json:"at_ms"`
		Action string          `json:"action"`
		Params json.RawMessage `json:"params"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		return Action{}, fmt.Errorf("malformed JSON: %w", err)
	}
	// Reject trailing data after the first object — one action per line.
	// Without this, json.Decoder silently ignores anything after the first value.
	if dec.More() {
		return Action{}, fmt.Errorf("unexpected trailing data after action object (one action per line)")
	}
	if env.AtMs < 0 {
		return Action{}, fmt.Errorf("at_ms must be >= 0, got %d", env.AtMs)
	}
	// Bound at_ms below the overflow point of time.Duration(at_ms)*time.Millisecond
	// so a typo'd giant timestamp fails loudly instead of wrapping negative and
	// firing immediately.
	if env.AtMs > maxAtMs {
		return Action{}, fmt.Errorf("at_ms %d exceeds maximum %d ms", env.AtMs, maxAtMs)
	}

	a := Action{AtMs: env.AtMs, Action: env.Action}
	switch env.Action {
	case actionPriceShift:
		var p priceShiftParams
		if err := decodeParams(env.Params, &p); err != nil {
			return Action{}, fmt.Errorf("price_shift params: %w", err)
		}
		scale := 0.0
		if p.Scale != nil {
			scale = *p.Scale
		}
		// NewPriceShift collapses non-finite / non-positive factors to identity,
		// so a hostile offset_bps/scale can never wreck the book.
		a.priceShift = NewPriceShift(p.OffsetBps, scale)
	case actionLatency:
		var p latencyParams
		if err := decodeParams(env.Params, &p); err != nil {
			return Action{}, fmt.Errorf("latency params: %w", err)
		}
		if p.FeedToBookMs < 0 || p.OrderAckMs < 0 || p.FillReportMs < 0 || p.JitterMs < 0 {
			return Action{}, fmt.Errorf("latency ms fields must be >= 0")
		}
		a.latency = LatencyConfig{
			FeedToBook: time.Duration(p.FeedToBookMs) * time.Millisecond,
			OrderAck:   time.Duration(p.OrderAckMs) * time.Millisecond,
			FillReport: time.Duration(p.FillReportMs) * time.Millisecond,
			Jitter:     time.Duration(p.JitterMs) * time.Millisecond,
		}
	case "":
		return Action{}, fmt.Errorf("missing action")
	default:
		return Action{}, fmt.Errorf("unknown action %q (want %q|%q)", env.Action, actionPriceShift, actionLatency)
	}
	return a, nil
}

// decodeParams decodes a params payload strictly (unknown keys rejected). A
// missing params object is treated as an empty object so every field defaults.
func decodeParams(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// LoadScenario reads and parses a scenario file from path.
func LoadScenario(path string, seed int64) (*Scenario, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open scenario: %w", err)
	}
	defer f.Close()
	return ParseScenario(f, seed)
}

// apply mutates c per the action a. The action was fully decoded at parse time,
// so this never re-parses or fails.
func (sc *Scenario) apply(c *Controls, a Action) {
	switch a.Action {
	case actionPriceShift:
		c.SetPriceShift(a.priceShift)
		slog.Info("scenario action applied",
			"action", a.Action, "at_ms", a.AtMs,
			"offset_bps", a.priceShift.OffsetBps, "scale", a.priceShift.Scale)
	case actionLatency:
		c.SetLatency(NewLatency(a.latency, sc.Seed))
		slog.Info("scenario action applied",
			"action", a.Action, "at_ms", a.AtMs,
			"feed_to_book_ms", a.latency.FeedToBook.Milliseconds(),
			"jitter_ms", a.latency.Jitter.Milliseconds())
	}
}

// applyDue applies every not-yet-fired event whose AtMs <= elapsed, in
// timeline order, mutating c. It is the deterministic core of the runner:
// tests drive it with a synthetic elapsed sequence (no real sleeps), and Run
// drives it from a ticker. Calling it with a monotonically non-decreasing
// elapsed fires each event exactly once. It returns the number of events fired
// on this call.
//
// elapsed is the time since the scenario start, already adjusted for speed by
// the caller (Run divides at_ms by speed; tests pass raw at_ms thresholds).
func (sc *Scenario) applyDue(c *Controls, elapsed time.Duration) int {
	fired := 0
	for sc.fired < len(sc.events) {
		ev := sc.events[sc.fired]
		if time.Duration(ev.AtMs)*time.Millisecond > elapsed {
			break // events are sorted; nothing later is due yet
		}
		sc.apply(c, ev)
		sc.fired++
		fired++
	}
	return fired
}

// Run drives the timeline against the wall clock until every event has fired or
// ctx is cancelled. Each event fires at start + at_ms/speed (speed default 1;
// speed > 1 accelerates, e.g. speed=10 runs a 10s timeline in 1s). The schedule
// is a pure function of (start, at_ms, speed), so a run is reproducible.
//
// Implementation: a per-event timer loop (not a polling ticker) so firing is
// precise and Run sleeps exactly until the next event. applyDue is reused so
// the firing semantics are identical to the test path.
func (sc *Scenario) Run(ctx context.Context, c *Controls, start time.Time, speed float64) error {
	if !finite(speed) || speed <= 0 {
		speed = 1
	}
	slog.Info("scenario started", "events", len(sc.events), "speed", speed)

	for sc.fired < len(sc.events) {
		next := sc.events[sc.fired]
		// Wall-clock instant this event is due: start + at_ms/speed.
		offset := time.Duration(float64(next.AtMs) * float64(time.Millisecond) / speed)
		if !finite(float64(offset)) {
			offset = 0
		}
		fireAt := start.Add(offset)
		wait := time.Until(fireAt)

		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		} else {
			// Already due (e.g. at_ms 0, or we fell behind) — check cancellation
			// without sleeping so a cancelled ctx stops us promptly.
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		// Fire everything now due, in timeline order (coalesces simultaneous /
		// behind-schedule events deterministically via applyDue).
		elapsed := time.Duration(float64(time.Since(start)) * speed)
		sc.applyDue(c, elapsed)
	}

	slog.Info("scenario complete", "events", len(sc.events))
	return nil
}
