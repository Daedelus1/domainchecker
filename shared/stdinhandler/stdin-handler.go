package stdinhandler

import (
	"bufio"
	"io"
	"time"
)

type StampedMessage struct {
	Msg       string
	Timestamp time.Time
}

type StdinHandler struct {
	stdin chan StampedMessage
	// done is closed by Close to tell readStdin to stop; stopped is closed
	// by readStdin once it actually has, letting Close wait for that before
	// closing stdin itself -- closing stdin directly, without first
	// confirming readStdin has stopped, would race against a concurrent
	// send to it.
	done    chan struct{}
	stopped chan struct{}
}

// New constructs a handler that reads newline-delimited input from r,
// stamping each line with the time it was read, and immediately starts
// reading it in the background. Each call gets its own independent
// handler and Stream() channel -- unlike a shared singleton, this lets a
// test (or any other caller) supply its own reader, e.g. a strings.Reader,
// instead of competing with everyone else for os.Stdin.
func New(r io.Reader) *StdinHandler {
	s := &StdinHandler{
		stdin:   make(chan StampedMessage),
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	go s.readStdin(r)
	return s
}

// Stream returns the channel every line read from r is published to.
func (s *StdinHandler) Stream() chan StampedMessage {
	return s.stdin
}

// Close stops the background reader and closes Stream's channel, blocking
// until readStdin has confirmed it will never send to it again -- so the
// close can never race with a pending send. If readStdin is currently
// blocked waiting for the next line of input (the common case), Close does
// not interrupt that pending read on r itself, and so will not return
// until r produces another line, reaches EOF, or errors. Must be called at
// most once.
func (s *StdinHandler) Close() {
	close(s.done)
	<-s.stopped
	close(s.stdin)
}

// readStdin runs for the life of the handler, publishing each line read
// from r (stamped with the time it was read) to Stream until Close is
// called or r reaches EOF or an error. Must run on its own goroutine;
// started once by New.
func (s *StdinHandler) readStdin(r io.Reader) {
	defer close(s.stopped)

	scanner := bufio.NewScanner(r)
	for scanner.Err() == nil && scanner.Scan() {
		line := scanner.Text()
		msg := StampedMessage{line, time.Now()}
		select {
		case s.stdin <- msg:
		case <-s.done:
			return
		}
	}
}
