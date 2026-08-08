package channel

// ConnectionBudget is momo's allowance of the requests it holds open at once,
// across channels: the max_connections the operator configured. A response a
// channel keeps parked holds a slot for as long as the client keeps it.
type ConnectionBudget struct{ slots chan struct{} }

func NewConnectionBudget(max int) *ConnectionBudget {
	return &ConnectionBudget{slots: make(chan struct{}, max)}
}

// Acquire takes a slot without waiting: a client at the limit is told to come
// back rather than left holding a request momo has not started answering.
func (b *ConnectionBudget) Acquire() (release func(), ok bool) {
	select {
	case b.slots <- struct{}{}:
		return func() { <-b.slots }, true
	default:
		return nil, false
	}
}
