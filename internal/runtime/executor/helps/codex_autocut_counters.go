package helps

import "sync/atomic"

var codexCompactHijackCounter atomic.Int64

// RecordCodexCompactHijack increments the global counter when a client /compact
// request targeting Codex is rerouted to an external summarizer model.
func RecordCodexCompactHijack() {
	codexCompactHijackCounter.Add(1)
}

// CodexCompactHijackCount returns the total number of hijacked client /compact turns.
func CodexCompactHijackCount() int64 {
	return codexCompactHijackCounter.Load()
}
