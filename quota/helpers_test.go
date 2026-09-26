package quota

import "time"

func testNow() time.Time { return time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC) }

func (m *Manager) currentSnap() *Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snap
}
