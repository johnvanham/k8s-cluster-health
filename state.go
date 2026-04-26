package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// persistentState is the on-disk record of cumulative incident bookkeeping
// for a given kubeconfig context. Stored at <stateDir>/<context>.json. The
// markdown report (in -log-dir) remains the source of truth for incident
// detail; this file exists only so the footer can show "last incident: …"
// across program restarts and so the incident counter survives a session.
type persistentState struct {
	Context            string    `json:"context"`
	LastIncidentEnd    time.Time `json:"last_incident_end,omitempty"`
	LastIncidentDur    string    `json:"last_incident_duration,omitempty"`
	LastIncidentReason string    `json:"last_incident_reason,omitempty"`
	IncidentCount      int       `json:"incident_count"`
}

// resolveStateDir picks the directory used for per-context state files.
// Explicit override (from -state-dir) wins; otherwise XDG_STATE_HOME, with
// ~/.local/state as the final fallback.
func resolveStateDir(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "k8s-cluster-health"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "k8s-cluster-health"), nil
}

// sanitizeContextName makes a context name safe to use as a filename.
// Kubeconfig contexts can contain ':' (LKE / EKS / GKE all use them),
// '/', and other path-unfriendly characters.
func sanitizeContextName(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		b := name[i]
		switch {
		case b >= 'a' && b <= 'z',
			b >= 'A' && b <= 'Z',
			b >= '0' && b <= '9',
			b == '.', b == '_', b == '-':
			out = append(out, b)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "default"
	}
	return string(out)
}

func stateFilePath(dir, ctxName string) string {
	return filepath.Join(dir, sanitizeContextName(ctxName)+".json")
}

func loadState(path string) (*persistentState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &persistentState{}, nil
		}
		return nil, err
	}
	var s persistentState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// saveState writes the state atomically (tmp + rename) so a SIGKILL mid-write
// can't leave a half-written JSON file.
func saveState(path string, s *persistentState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
