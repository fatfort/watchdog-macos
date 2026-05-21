package typtel

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

// withStubs swaps both lookPath and runCommand for the duration of a test
// and restores the originals on cleanup. Centralised here so individual
// tests stay focused on the contract rather than plumbing.
func withStubs(t *testing.T, lp func(string) (string, error), rc func(context.Context, string) ([]byte, error)) {
	t.Helper()
	origLP, origRC := lookPath, runCommand
	lookPath, runCommand = lp, rc
	t.Cleanup(func() { lookPath, runCommand = origLP, origRC })
}

func TestStatsAbsentBinary(t *testing.T) {
	withStubs(t,
		func(string) (string, error) { return "", exec.ErrNotFound },
		nil,
	)
	s, ok, err := Fetch()
	if err != nil {
		t.Fatalf("expected nil error when typtel is missing, got %v", err)
	}
	if ok {
		t.Fatal("expected ok=false when typtel is missing")
	}
	if s != (Stats{}) {
		t.Fatalf("expected zero stats, got %+v", s)
	}
}

func TestStatsSuccess(t *testing.T) {
	const payload = `{"date":"2026-05-21","keystrokes":1234,"words":300,"letters":900,"modifiers":100,"special":234,"mouse_clicks":40,"mouse_distance_px":5000,"mouse_distance_m":1.27,"active_hours":3}`
	withStubs(t,
		func(string) (string, error) { return "/usr/local/bin/typtel", nil },
		func(context.Context, string) ([]byte, error) { return []byte(payload), nil },
	)

	s, ok, err := Fetch()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if s.Keystrokes != 1234 || s.Words != 300 || s.MouseClicks != 40 || s.ActiveHours != 3 {
		t.Fatalf("decoded fields wrong: %+v", s)
	}
}

func TestStatsBadJSON(t *testing.T) {
	withStubs(t,
		func(string) (string, error) { return "/usr/local/bin/typtel", nil },
		func(context.Context, string) ([]byte, error) { return []byte("not json"), nil },
	)
	_, ok, err := Fetch()
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
	if ok {
		t.Fatal("expected ok=false on parse failure")
	}
}

func TestStatsRunError(t *testing.T) {
	withStubs(t,
		func(string) (string, error) { return "/usr/local/bin/typtel", nil },
		func(context.Context, string) ([]byte, error) { return nil, errors.New("boom") },
	)
	_, ok, err := Fetch()
	if err == nil {
		t.Fatal("expected error when subprocess fails")
	}
	if ok {
		t.Fatal("expected ok=false when subprocess fails")
	}
}
