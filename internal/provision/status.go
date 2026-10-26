package provision

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// StatusWriter records diagnostics, never a cache or a reason to skip steps.
// Callers provide the raw document's hash; no payloads or error text are stored.
func StatusWriter(path, configHash string) func(string, string) error {
	started := time.Now().UTC()
	return func(state, step string) error {
		status := struct {
			Version      int       `json:"version"`
			ConfigSHA256 string    `json:"configSHA256"`
			State        string    `json:"state"`
			Step         string    `json:"step,omitempty"`
			StartedAt    time.Time `json:"startedAt"`
			UpdatedAt    time.Time `json:"updatedAt"`
		}{1, configHash, state, step, started, time.Now().UTC()}
		data, err := json.Marshal(status)
		if err != nil {
			return err
		}
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		return atomicFile(path, append(data, '\n'), 0600)
	}
}
