package server

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/notshekhar/drover/internal/api"
	"github.com/notshekhar/drover/internal/config"
	"github.com/notshekhar/drover/internal/object"
	"github.com/notshekhar/drover/internal/tui"
)

// Dashboard adapts the engine to what the TUI needs to draw and to the two
// actions it offers.
type Dashboard struct {
	srv     *Server
	cfg     *config.Config
	started time.Time
	listen  string

	// The watcher reloads on its own, so its result is stashed here for the
	// next repaint to show. Guarded because the watcher runs on its own
	// goroutine.
	mu         sync.Mutex
	notice     string
	noticeKind string
	noticeAt   time.Time
}

// NewDashboard returns a tui.Source over this server.
func (s *Server) NewDashboard(cfg *config.Config, listen string) *Dashboard {
	return &Dashboard{srv: s, cfg: cfg, started: time.Now(), listen: listen}
}

// Snapshot is the tui.Source method: it reads state and shapes it for the
// screen. Going through the same wire type the HTTP endpoint returns means
// `drover serve` and `drover dash` render from identical data.
func (d *Dashboard) Snapshot() tui.Model {
	state := d.srv.DashboardState(d.listen, d.started)
	m := tui.FromState(state, d.started)

	d.mu.Lock()
	defer d.mu.Unlock()
	// A reload the watcher did should be visible for a few seconds, the same
	// as one someone asked for.
	if d.notice != "" && time.Since(d.noticeAt) < 8*time.Second {
		m.Notice, m.NoticeKind = d.notice, d.noticeKind
	}
	return m
}

// NoteReload records what an automatic reload did, for the next repaint.
func (d *Dashboard) NoteReload(msg string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.noticeAt = time.Now()
	if err != nil {
		d.notice, d.noticeKind = err.Error(), "err"
		return
	}
	d.notice, d.noticeKind = msg, "ok"
}

// DashboardState reads current state off disk.
//
// It runs on every repaint, so it only touches the store and the status files
// -- never the network, and never a database. A dashboard that stalls because
// a remote is down would be worse than no dashboard.
func (s *Server) DashboardState(listen string, started time.Time) api.DashboardResponse {
	m := api.DashboardResponse{
		Version:   s.opts.Version,
		Listen:    listen,
		DataDir:   s.opts.DataDir,
		UptimeSec: int64(time.Since(started).Seconds()),
	}

	if repos, err := s.store.List(object.KindRepository); err == nil {
		for _, o := range repos {
			row := api.DashRepo{Name: o.Metadata.Name}
			if spec, err := o.Repository(); err == nil {
				row.URL = spec.URL
				row.Branch = spec.Branch
				row.Refresh = spec.RefreshInterval.String()
				if !spec.RefreshInterval.Set {
					row.Refresh = s.opts.Sync.String()
				}
			}
			if st, err := s.store.GetStatus(object.KindRepository, o.Metadata.Name); err == nil {
				row.Status = string(st.Phase)
				row.Commit = st.Commit
				row.Error = st.Error
				row.LastSync = st.LastSuccess
			}
			m.Repos = append(m.Repos, row)
		}
	}

	if reqs, err := s.store.List(object.KindHTTPRequest); err == nil {
		for _, o := range reqs {
			row := api.DashRequest{Name: o.Metadata.Name}
			if spec, err := o.HTTPRequest(); err == nil {
				row.Method = spec.NormalizedMethod()
				row.URL = spec.URL
				row.Offered = spec.IsSafe()
				row.Environment = spec.DefaultEnvironment
				if row.Environment == "" && len(spec.Environments) == 1 {
					row.Environment = spec.Environments[0]
				}
				if row.Environment == "" {
					row.Environment = "-"
				}
			}
			m.Requests = append(m.Requests, row)
		}
	}

	if sqls, err := s.store.List(object.KindSQLConnection); err == nil {
		for _, o := range sqls {
			row := api.DashSQL{Name: o.Metadata.Name}
			if spec, err := o.SQLConnection(); err == nil {
				if p, err := spec.ResolveProvider(); err == nil {
					row.Provider = string(p)
				}
				row.ReadOnly = spec.IsReadOnly()
			}
			if st, err := s.store.GetStatus(object.KindSQLConnection, o.Metadata.Name); err == nil {
				row.Status = string(st.Phase)
				row.Error = st.Error
			}
			m.SQLs = append(m.SQLs, row)
		}
	}

	if envs, err := s.store.List(object.KindEnvironment); err == nil {
		for _, o := range envs {
			row := api.DashEnv{Name: o.Metadata.Name}
			if spec, err := o.Environment(); err == nil {
				row.Variables = len(spec.Variables)
				row.Secrets = len(spec.Secrets)
				// An unset secret is a call that will fail later, so it is
				// worth surfacing before anybody makes that call.
				for _, st := range spec.SecretStatuses(nil) {
					if !st.Set {
						row.Unset++
					}
				}
			}
			m.Envs = append(m.Envs, row)
		}
	}

	return m
}

// Reload re-reads the config and applies it. The dashboard's button and the
// HTTP endpoint both land here.
func (d *Dashboard) Reload() (string, error) {
	msg, cfg, err := d.srv.Reload(d.cfg.Path())
	if err != nil {
		return "", err
	}
	d.cfg = cfg
	return msg, nil
}

// Reload re-reads every path in the config's apply list and applies it.
//
// This is what "I edited the yaml" needs. It deliberately re-reads
// config.yaml from disk too, so adding a path to apply: and reloading picks
// it up without a restart.
func (s *Server) Reload(cfgPath string) (string, *config.Config, error) {
	fresh, err := config.Load(cfgPath)
	if err != nil {
		return "", nil, fmt.Errorf("config: %w", err)
	}

	// Same sources as a startup apply: the yaml in the data directory plus
	// the config's paths. A reload that saw less than a restart would be a
	// trap.
	files, err := s.sourceFiles(fresh)
	if err != nil {
		return "", nil, err
	}
	if len(files) == 0 {
		return fmt.Sprintf("nothing to reload: no yaml in %s and no paths in apply:", s.opts.DataDir), fresh, nil
	}

	batch, err := s.readBatch(files)
	if err != nil {
		return "", nil, err
	}
	if err := batch.Check(s.storedRefs()); err != nil {
		return "", nil, err
	}

	created, updated := 0, 0
	for _, o := range batch.Objects {
		if _, err := s.store.Get(o.Kind, o.Metadata.Name); err == nil {
			updated++
		} else {
			created++
		}
		if err := s.store.Put(o); err != nil {
			return "", nil, err
		}
		// Picks up a changed url, branch or refreshInterval immediately, and
		// drops the pooled connection of a changed database. Same hook the
		// apply and put routes use, so reloading a file and applying it by
		// hand cannot drift apart.
		s.objectChanged(o)
	}

	// A connection that was failing may be reachable now.
	go s.recheckSQL(batch.Objects)

	return fmt.Sprintf("reloaded %d object(s) from %d file(s): %d created, %d updated",
		batch.Len(), len(files), created, updated), fresh, nil
}

func (s *Server) recheckSQL(objs []*object.Object) {
	for _, o := range objs {
		if o.Kind != object.KindSQLConnection {
			continue
		}
		name := o.Metadata.Name
		spec, err := o.SQLConnection()
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_ = s.checkSQLHealth(ctx, name, spec)
		cancel()
	}
}

// SyncAll asks every repository to refresh now.
func (d *Dashboard) SyncAll() error {
	if d.srv.sync == nil {
		return fmt.Errorf("this engine was started without sync")
	}
	d.srv.sync.SyncAll()
	return nil
}
