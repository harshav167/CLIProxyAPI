package redisqueue

import (
	"testing"
	"time"
)

func TestEnqueueBroadcastsToUsageSubscribersAndSkipsQueue(t *testing.T) {
	withEnabledQueue(t, func() {
		first, unsubscribeFirst := SubscribeUsage()
		defer unsubscribeFirst()
		second, unsubscribeSecond := SubscribeUsage()
		defer unsubscribeSecond()

		Enqueue([]byte("usage-record"))

		requireUsageSubscriberPayload(t, first, "usage-record")
		requireUsageSubscriberPayload(t, second, "usage-record")

		if items := PopOldest(1); len(items) != 0 {
			t.Fatalf("PopOldest() items = %q, want empty after subscriber broadcast", items)
		}

		unsubscribeFirst()
		unsubscribeSecond()

		Enqueue([]byte("queued-record"))
		items := PopOldest(1)
		if len(items) != 1 || string(items[0]) != "queued-record" {
			t.Fatalf("PopOldest() items = %q, want queued record after unsubscribe", items)
		}
	})
}

func TestEnqueuePersistsWhenSubscriberBufferFull(t *testing.T) {
	withEnabledQueue(t, func() {
		subscriber, unsubscribe := SubscribeUsage()
		defer unsubscribe()

		// Fill the subscriber's buffer so the next Enqueue cannot deliver.
		for i := 0; i < usageSubscriberBuffer; i++ {
			Enqueue([]byte("fill"))
		}

		// Drain everything currently buffered so we can isolate the overflow
		// record below. The buffer now holds usageSubscriberBuffer "fill"
		// records (deliveries succeeded, so none hit the backend yet).
		for i := 0; i < usageSubscriberBuffer; i++ {
			select {
			case <-subscriber:
			case <-time.After(time.Second):
				t.Fatalf("timeout draining buffered fill record %d", i)
			}
		}

		// Re-fill the buffer to capacity, then enqueue one more. The overflow
		// record cannot be delivered (buffer full) and must be persisted to the
		// backend rather than dropped.
		for i := 0; i < usageSubscriberBuffer; i++ {
			Enqueue([]byte("fill2"))
		}
		Enqueue([]byte("overflow-record"))

		items := PopOldest(usageSubscriberBuffer + 1)
		found := false
		for _, item := range items {
			if string(item) == "overflow-record" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("overflow record was lost; backend items = %q", items)
		}
	})
}

func TestSetEnabledFalseClosesUsageSubscribers(t *testing.T) {
	withEnabledQueue(t, func() {
		subscriber, unsubscribe := SubscribeUsage()
		defer unsubscribe()

		SetEnabled(false)

		select {
		case _, ok := <-subscriber:
			if ok {
				t.Fatalf("subscriber channel remained open after SetEnabled(false)")
			}
		case <-time.After(time.Second):
			t.Fatalf("timeout waiting for subscriber close")
		}
	})
}

func requireUsageSubscriberPayload(t *testing.T, subscriber <-chan []byte, want string) {
	t.Helper()

	select {
	case got, ok := <-subscriber:
		if !ok {
			t.Fatalf("subscriber closed before receiving %q", want)
		}
		if string(got) != want {
			t.Fatalf("subscriber payload = %q, want %q", string(got), want)
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for subscriber payload %q", want)
	}
}
