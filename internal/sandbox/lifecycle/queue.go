package lifecycle

import (
	"sync"

	"github.com/runplane/runplane/internal/sandbox"
)

// queue delivers events in order. It has no size limit, so a slow reader of
// the events never blocks a sandbox.
type queue struct {
	out  chan sandbox.Event
	wake chan struct{}
	quit chan struct{}
	done chan struct{}

	mu   sync.Mutex
	list []sandbox.Event
}

func newQueue() *queue {
	q := &queue{
		out:  make(chan sandbox.Event),
		wake: make(chan struct{}, 1),
		quit: make(chan struct{}),
		done: make(chan struct{}),
	}
	go q.run()
	return q
}

func (q *queue) push(ev sandbox.Event) {
	q.mu.Lock()
	q.list = append(q.list, ev)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *queue) run() {
	defer close(q.done)
	defer close(q.out)
	for {
		q.mu.Lock()
		batch := q.list
		q.list = nil
		q.mu.Unlock()
		for _, ev := range batch {
			select {
			case q.out <- ev:
			case <-q.quit:
				return
			}
		}
		select {
		case <-q.wake:
		case <-q.quit:
			return
		}
	}
}

// close ends the delivery and closes the out channel.
func (q *queue) close() {
	close(q.quit)
	<-q.done
}
