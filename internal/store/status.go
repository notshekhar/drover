package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/notshekhar/drover/internal/atomicfile"
	"github.com/notshekhar/drover/internal/object"
)

// Phase is where a reconcile got to.
type Phase string

const (
	PhasePending Phase = "pending" // applied, not reconciled yet
	PhaseSyncing Phase = "syncing"
	PhaseReady   Phase = "ready"
	PhaseFailed  Phase = "failed"
)

// Status is observed state: what the engine found when it last tried.
//
// It is stored apart from the object because the object is desired state and
// should read back exactly as applied. Mixing the two means `get -o yaml`
// hands back something you cannot re-apply.
type Status struct {
	Phase       Phase  `yaml:"phase"`
	Commit      string `yaml:"commit,omitempty"`
	Branch      string `yaml:"branch,omitempty"`
	LastAttempt string `yaml:"lastAttempt,omitempty"`
	LastSuccess string `yaml:"lastSuccess,omitempty"`
	Error       string `yaml:"error,omitempty"`
}

func (s *Store) statusPath(kind object.Kind, name string) string {
	return filepath.Join(filepath.Dir(s.dir), "status", string(kind), name+".yaml")
}

// SetStatus records observed state for one object.
func (s *Store) SetStatus(kind object.Kind, name string, st *Status) error {
	if err := object.ValidateName(name); err != nil {
		return fmt.Errorf("metadata.name: %w", err)
	}
	data, err := yaml.Marshal(st)
	if err != nil {
		return err
	}
	return atomicfile.Write(s.statusPath(kind, name), data, 0o644)
}

// GetStatus reads observed state. An object that has never been reconciled
// reports pending rather than an error -- not having run yet is a state, not
// a failure.
func (s *Store) GetStatus(kind object.Kind, name string) (*Status, error) {
	data, err := os.ReadFile(s.statusPath(kind, name))
	if errors.Is(err, os.ErrNotExist) {
		return &Status{Phase: PhasePending}, nil
	}
	if err != nil {
		return nil, err
	}
	var st Status
	if err := yaml.Unmarshal(data, &st); err != nil {
		// A corrupt status file must not take the engine down: unlike an
		// object, status can be rebuilt by simply reconciling again.
		return &Status{Phase: PhasePending}, nil
	}
	return &st, nil
}

// DeleteStatus drops observed state, for when its object goes away.
func (s *Store) DeleteStatus(kind object.Kind, name string) error {
	err := os.Remove(s.statusPath(kind, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// MarkSyncing records that an attempt has started, so a long clone is visible
// as work in progress rather than looking stuck at pending.
func (s *Store) MarkSyncing(kind object.Kind, name string) error {
	st, err := s.GetStatus(kind, name)
	if err != nil {
		return err
	}
	st.Phase = PhaseSyncing
	st.LastAttempt = now()
	return s.SetStatus(kind, name, st)
}

// MarkReady records a successful reconcile.
func (s *Store) MarkReady(kind object.Kind, name, commit, branch string) error {
	ts := now()
	return s.SetStatus(kind, name, &Status{
		Phase:       PhaseReady,
		Commit:      commit,
		Branch:      branch,
		LastAttempt: ts,
		LastSuccess: ts,
	})
}

// MarkFailed records a failure, keeping the last success so the table can say
// how long a repository has been broken.
func (s *Store) MarkFailed(kind object.Kind, name string, cause error) error {
	st, err := s.GetStatus(kind, name)
	if err != nil {
		st = &Status{}
	}
	st.Phase = PhaseFailed
	st.LastAttempt = now()
	st.Error = cause.Error()
	return s.SetStatus(kind, name, st)
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }
