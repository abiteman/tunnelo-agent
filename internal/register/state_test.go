package register

import (
	"os"
	"testing"
)

// A pre-multi-service state file (single subdomain/port, no services array) is
// migrated to a one-entry services list on load, so an upgrade isn't seen as a
// changed service set.
func TestLoadStateMigratesSingleService(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/state.json", []byte(`{
		"agent_id":"agt_1","agent_secret":"as_1","subdomain":"falcon",
		"private_key":"k","service_port":8096
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(s.Services) != 1 || s.Services[0].Subdomain != "falcon" || s.Services[0].Port != 8096 {
		t.Fatalf("migrated services = %+v, want one falcon:8096", s.Services)
	}
	if got := s.Ports(); len(got) != 1 || got[0] != 8096 {
		t.Fatalf("Ports() = %v, want [8096] so an upgrade isn't a service-set change", got)
	}
}
