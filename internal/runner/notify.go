package runner

import "sync"

// notifier wakes long polls waiting on an agent. Every poll of one agent
// waits on the same channel, which notify closes.
type notifier struct {
	mu      sync.Mutex
	waiters map[string]chan struct{}
}

func newNotifier() *notifier { return &notifier{waiters: map[string]chan struct{}{}} }

func (n *notifier) wait(agentID string) <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	ch, ok := n.waiters[agentID]
	if !ok {
		ch = make(chan struct{})
		n.waiters[agentID] = ch
	}
	return ch
}

func (n *notifier) notify(agentID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if ch, ok := n.waiters[agentID]; ok {
		close(ch)
		delete(n.waiters, agentID)
	}
}
