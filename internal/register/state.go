package register

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// State is everything the agent persists between runs: credentials, the
// locally generated WireGuard private key, and the gateway-assigned config.
// It contains secrets and is written with mode 0600.
type State struct {
	AgentID           string          `json:"agent_id"`
	AgentSecret       string          `json:"agent_secret"`
	Subdomain         string          `json:"subdomain"`
	PrivateKey        string          `json:"private_key"`
	WireGuard         WireGuardConfig `json:"wireguard"`
	ServicePort       int             `json:"service_port"`
	HeartbeatInterval int             `json:"heartbeat_interval_seconds"`
	Speedtest         SpeedtestConfig `json:"speedtest"`
	SpeedtestDone     bool            `json:"speedtest_done"`
}

const stateFile = "state.json"

// LoadState reads persisted state from dir. It returns (nil, nil) when no
// state exists yet.
func LoadState(dir string) (*State, error) {
	data, err := os.ReadFile(filepath.Join(dir, stateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading state: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", stateFile, err)
	}
	return &s, nil
}

// Save writes the state atomically to dir with owner-only permissions.
func (s *State) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, stateFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing state: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, stateFile)); err != nil {
		return fmt.Errorf("committing state: %w", err)
	}
	return nil
}

// Remove deletes persisted state, e.g. after the gateway reports the agent
// credentials as revoked.
func Remove(dir string) error {
	err := os.Remove(filepath.Join(dir, stateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
