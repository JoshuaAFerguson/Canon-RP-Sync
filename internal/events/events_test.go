package events_test

import (
	"sync"
	"testing"
	"time"

	"github.com/JoshuaAFerguson/canon-rp-sync/internal/events"
)

func TestPublishReachesSubscribers(t *testing.T) {
	b := events.NewBus()
	ch, cancel := b.Subscribe(4)
	defer cancel()

	b.Publishf(events.KindCamera, "connected to %s", "192.168.1.42")

	select {
	case ev := <-ch:
		if ev.Kind != events.KindCamera {
			t.Errorf("Kind = %q", ev.Kind)
		}
		if ev.Message != "connected to 192.168.1.42" {
			t.Errorf("Message = %q", ev.Message)
		}
		if ev.Time.IsZero() {
			t.Error("Time should be stamped on publish")
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber received nothing")
	}
}

func TestSlowSubscriberDoesNotBlockPublisher(t *testing.T) {
	b := events.NewBus()
	_, cancel := b.Subscribe(1) // never read from
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			b.Publishf(events.KindImport, "event %d", i)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a subscriber that stopped reading blocked the publisher")
	}
}

func TestCancelUnsubscribes(t *testing.T) {
	b := events.NewBus()
	ch, cancel := b.Subscribe(1)
	if b.Subscribers() != 1 {
		t.Fatalf("Subscribers = %d", b.Subscribers())
	}

	cancel()
	cancel() // must be idempotent

	if b.Subscribers() != 0 {
		t.Errorf("Subscribers = %d after cancel", b.Subscribers())
	}
	if _, open := <-ch; open {
		t.Error("channel should be closed after cancel")
	}
}

func TestRecentKeepsLastEvents(t *testing.T) {
	b := events.NewBus()
	for i := 0; i < 150; i++ {
		b.Publishf(events.KindImport, "event %d", i)
	}

	recent := b.Recent(5)
	if len(recent) != 5 {
		t.Fatalf("Recent(5) returned %d", len(recent))
	}
	if recent[4].Message != "event 149" {
		t.Errorf("last event = %q", recent[4].Message)
	}
	if got := len(b.Recent(0)); got > 100 {
		t.Errorf("history grew to %d, want it bounded", got)
	}
}

// TestPublishRacesWithUnsubscribe reproduces a crash found by CI: Publish used
// to snapshot subscribers under the lock and send after releasing it, so an
// unsubscribe landing in that window closed the channel mid-send. A closed
// channel does not merely race — sending on it panics, which would take the
// daemon down when a browser dropped its event stream.
func TestPublishRacesWithUnsubscribe(t *testing.T) {
	b := events.NewBus()

	done := make(chan struct{})
	var publishers sync.WaitGroup
	publishers.Add(1)
	go func() {
		defer publishers.Done()
		for {
			select {
			case <-done:
				return
			default:
				b.Publishf(events.KindImport, "imported a file")
			}
		}
	}()

	// Subscribers come and go while events are being published, the way SSE
	// clients connect and disconnect.
	var churn sync.WaitGroup
	for i := 0; i < 50; i++ {
		churn.Add(1)
		go func() {
			defer churn.Done()
			ch, cancel := b.Subscribe(1)
			go func() {
				for range ch {
				}
			}()
			time.Sleep(time.Millisecond)
			cancel()
		}()
	}
	churn.Wait()
	close(done)
	publishers.Wait()

	if n := b.Subscribers(); n != 0 {
		t.Errorf("Subscribers = %d after every subscription was cancelled", n)
	}
}
