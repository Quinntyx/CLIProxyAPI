package sessionquota

func (m *Manager) HasAccounts() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.state.Accounts) > 0
}
