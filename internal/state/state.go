// Package state keeps the alert state file between runs: which anomalies have
// already been posted, so an hourly cron does not page six times for one
// spike.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/moveeeax/aws-cost-slack/internal/costs"
)

// Load reads the state file. A missing file is an empty state (first run).
func Load(path string) (costs.AlertState, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return costs.AlertState{Open: map[string]costs.OpenAlert{}}, nil
	}
	if err != nil {
		return costs.AlertState{}, fmt.Errorf("read state: %w", err)
	}
	var st costs.AlertState
	if err := json.Unmarshal(b, &st); err != nil {
		return costs.AlertState{}, fmt.Errorf("parse state %s: %w", path, err)
	}
	if st.Open == nil {
		st.Open = map[string]costs.OpenAlert{}
	}
	return st, nil
}

// Save writes the state file atomically (temp file + rename), so a run killed
// mid-write leaves the previous state intact.
func Save(path string, st costs.AlertState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".aws-cost-slack-state-*")
	if err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	return nil
}
