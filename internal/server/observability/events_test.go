// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package observability

import (
	"fmt"
	"sync"
	"testing"
)

func TestEventRing_BasicPushRecent(t *testing.T) {
	r := NewEventRing(5)

	r.PushEvent("info", "reconnect", "web-01", "stream reconnected", 0)
	r.PushEvent("warn", "rotate", "web-01", "flow rotated", 1)

	events := r.Recent(0)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Type != "reconnect" {
		t.Errorf("expected first event 'reconnect', got %q", events[0].Type)
	}
	if events[1].Type != "rotate" {
		t.Errorf("expected second event 'rotate', got %q", events[1].Type)
	}
}

func TestEventRing_Wrap(t *testing.T) {
	r := NewEventRing(3)

	for i := 0; i < 5; i++ {
		r.PushEvent("info", "test", "", fmt.Sprintf("event-%d", i), 0)
	}

	events := r.Recent(0)
	if len(events) != 3 {
		t.Fatalf("expected 3 events after wrap, got %d", len(events))
	}
	// Deve ter os últimos 3: event-2, event-3, event-4
	if events[0].Message != "event-2" {
		t.Errorf("expected 'event-2', got %q", events[0].Message)
	}
	if events[2].Message != "event-4" {
		t.Errorf("expected 'event-4', got %q", events[2].Message)
	}
}

func TestEventRing_Limit(t *testing.T) {
	r := NewEventRing(10)
	for i := 0; i < 8; i++ {
		r.PushEvent("info", "test", "", fmt.Sprintf("e%d", i), 0)
	}

	events := r.Recent(3)
	if len(events) != 3 {
		t.Fatalf("expected 3 events with limit, got %d", len(events))
	}
	// Últimos 3: e5, e6, e7
	if events[0].Message != "e5" {
		t.Errorf("expected 'e5', got %q", events[0].Message)
	}
}

func TestEventRing_Empty(t *testing.T) {
	r := NewEventRing(10)
	events := r.Recent(0)
	if len(events) != 0 {
		t.Errorf("expected empty, got %d", len(events))
	}
}

func TestEventRing_Len(t *testing.T) {
	r := NewEventRing(5)
	if r.Len() != 0 {
		t.Errorf("expected len 0, got %d", r.Len())
	}
	r.PushEvent("info", "test", "", "msg", 0)
	if r.Len() != 1 {
		t.Errorf("expected len 1, got %d", r.Len())
	}
	for i := 0; i < 10; i++ {
		r.PushEvent("info", "test", "", "msg", 0)
	}
	if r.Len() != 5 {
		t.Errorf("expected len capped at 5, got %d", r.Len())
	}
}

func TestEventRing_Concurrent(t *testing.T) {
	r := NewEventRing(100)
	var wg sync.WaitGroup

	// 10 writers concorrentes
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				r.PushEvent("info", "test", "", fmt.Sprintf("g%d-e%d", g, i), 0)
			}
		}(g)
	}

	// 5 readers concorrentes
	for g := 0; g < 5; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_ = r.Recent(10)
			}
		}()
	}

	wg.Wait()

	if r.Len() != 100 {
		t.Errorf("expected len 100 after 500 pushes in cap 100, got %d", r.Len())
	}
}

func TestEventRing_TimestampAutoFilled(t *testing.T) {
	r := NewEventRing(5)
	r.Push(EventEntry{Level: "info", Message: "no timestamp"})
	events := r.Recent(1)
	if events[0].Timestamp == "" {
		t.Error("expected auto-filled timestamp")
	}
}
