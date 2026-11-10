package service

import "maps"

// PreserveAccountRefillIdentity keeps a committed delivery associated with its
// supplier when an older edit form or credential refresh replaces extra.
// Initial adoption and verified repairs write these fields separately.
func PreserveAccountRefillIdentity(current, incoming map[string]any) map[string]any {
	if marker, _ := current["refill_job_id"].(string); marker == "" {
		return incoming
	}
	incoming = maps.Clone(incoming)
	if incoming == nil {
		incoming = make(map[string]any)
	}
	for _, key := range []string{"refill_job_id", "refill_supplier_id", "refill_plan", "import_source"} {
		delete(incoming, key)
		if value, ok := current[key]; ok {
			incoming[key] = value
		}
	}
	return incoming
}
