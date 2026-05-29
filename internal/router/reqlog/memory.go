package reqlog

import "sync"

// MemorySink buffers records in memory; used by router tests to assert that
// the right Record values reach the sink. Safe for concurrent Log calls.
type MemorySink struct {
	mu      sync.Mutex
	records []Record
}

// Log appends rec to the buffer.
func (m *MemorySink) Log(rec Record) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = append(m.records, rec)
}

// Close is a no-op for MemorySink.
func (m *MemorySink) Close() error { return nil }

// Records returns a snapshot of the buffered records.
func (m *MemorySink) Records() []Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Record, len(m.records))
	copy(out, m.records)
	return out
}

// Len reports the number of records buffered so far.
func (m *MemorySink) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.records)
}
