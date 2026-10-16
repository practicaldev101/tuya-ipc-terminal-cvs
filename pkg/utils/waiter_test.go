package utils

import (
	"errors"
	"testing"
	"time"
)

func TestWaiterReturnsErrorPassedToDone(t *testing.T) {
	for i := 0; i < 200; i++ {
		var w Waiter
		want := errors.New("boom")
		result := make(chan error, 1)
		go func() { result <- w.Wait() }()
		// Give Wait a chance to register before Done.
		time.Sleep(time.Microsecond)
		w.Done(want)
		select {
		case got := <-result:
			if !errors.Is(got, want) {
				t.Fatalf("iteration %d: Wait returned %v, want %v", i, got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("Wait did not return")
		}
	}
}

func TestWaiterDoneBeforeWait(t *testing.T) {
	var w Waiter
	want := errors.New("stopped")
	w.Done(want)
	if got := w.Wait(); !errors.Is(got, want) {
		t.Fatalf("Wait after Done returned %v, want %v", got, want)
	}
	// Later Done calls must not change the result.
	w.Done(nil)
	if got := w.Wait(); !errors.Is(got, want) {
		t.Fatalf("Wait after second Done returned %v, want %v", got, want)
	}
}
