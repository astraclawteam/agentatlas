package outcome

// NodeSummaryForTest exposes a MemoryStore lineage node's stored summary by id
// so the shared conformance suite can prove append-only node immutability
// (first-write-wins) without adding a production lineage-node getter -- a node
// reader/traversal is Task 0I scope. Returns (summary, found).
func (s *MemoryStore) NodeSummaryForTest(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[id]
	return n.Summary, ok
}
