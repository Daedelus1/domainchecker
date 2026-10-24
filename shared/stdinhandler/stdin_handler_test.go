package stdinhandler_test

import (
	"io"
	"strings"
	"testing"
	"time"

	"ethanstewart.net/domainchecker/shared/stdinhandler"
)

func TestStreamDeliversLinesInOrder(t *testing.T) {
	before := time.Now()
	h := stdinhandler.New(strings.NewReader("first\nsecond\nthird\n"))

	want := []string{"first", "second", "third"}
	for i, line := range want {
		select {
		case msg := <-h.Stream():
			if msg.Msg != line {
				t.Errorf("line %d = %q, want %q", i, msg.Msg, line)
			}
			if msg.Timestamp.Before(before) {
				t.Errorf("line %d Timestamp = %v, want it at or after %v", i, msg.Timestamp, before)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for line %d (%q)", i, line)
		}
	}
}

func TestStreamEndsAtEOF(t *testing.T) {
	h := stdinhandler.New(strings.NewReader("only line\n"))

	select {
	case msg := <-h.Stream():
		if msg.Msg != "only line" {
			t.Fatalf("Msg = %q, want %q", msg.Msg, "only line")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the only line")
	}

	// The reader is exhausted; nothing further should ever arrive.
	select {
	case msg, ok := <-h.Stream():
		t.Errorf("received %+v (ok=%v) after EOF, want no further messages", msg, ok)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCloseAbortsPendingSend(t *testing.T) {
	r, w := io.Pipe()
	h := stdinhandler.New(r)

	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	// Give readStdin a moment to scan the line and reach the select in its
	// loop body, blocked offering it to Stream (which nothing here
	// drains), so Close below has something to abort rather than finding
	// readStdin idle.
	time.Sleep(50 * time.Millisecond)

	closeDone := make(chan struct{})
	go func() {
		h.Close()
		close(closeDone)
	}()

	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close() did not return")
	}

	select {
	case _, ok := <-h.Stream():
		if ok {
			t.Errorf("received a message after Close(), want the channel closed")
		}
	case <-time.After(time.Second):
		t.Fatal("Stream() did not reflect Close() promptly")
	}
}

func TestCloseWaitsForBlockedReadToUnblock(t *testing.T) {
	// Never written to, so readStdin starts out blocked inside r.Read,
	// with nothing yet scanned and so nothing for Close to abort via the
	// done channel -- Close can only return once that read itself
	// unblocks.
	r, w := io.Pipe()
	h := stdinhandler.New(r)

	closeDone := make(chan struct{})
	go func() {
		h.Close()
		close(closeDone)
	}()

	select {
	case <-closeDone:
		t.Fatal("Close() returned before the blocked read on r unblocked")
	case <-time.After(100 * time.Millisecond):
	}

	if err := w.Close(); err != nil {
		t.Fatalf("w.Close() error = %v", err)
	}

	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close() did not return after the blocked read unblocked")
	}

	select {
	case _, ok := <-h.Stream():
		if ok {
			t.Errorf("received a message after Close(), want the channel closed")
		}
	case <-time.After(time.Second):
		t.Fatal("Stream() did not reflect Close() promptly")
	}
}
