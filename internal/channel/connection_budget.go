package channel

// ConnectionBudget is momo's allowance of the long-lived connections it holds
// open at once, across channels: the max_connections the operator configured.
// Each one occupies a server connection and a goroutine for as long as the
// client keeps it, so the ceiling belongs to momo as a whole rather than to each
// channel on its own.
type ConnectionBudget struct{ slots chan struct{} }

func NewConnectionBudget(max int) *ConnectionBudget {
	return &ConnectionBudget{slots: make(chan struct{}, max)}
}

// Max is the allowance momo was configured with.
func (b *ConnectionBudget) Max() int { return cap(b.slots) }

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
