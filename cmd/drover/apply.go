package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/notshekhar/drover/internal/api"
	"github.com/notshekhar/drover/internal/client"
	"github.com/notshekhar/drover/internal/config"
	"github.com/notshekhar/drover/internal/object"
)

// repeatable collects a flag that may be given more than once.
type repeatable []string

func (r *repeatable) String() string     { return fmt.Sprint(*r) }
func (r *repeatable) Set(v string) error { *r = append(*r, v); return nil }

func cmdApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var paths repeatable
	fs.Var(&paths, "f", "file or directory to apply (repeatable; - for stdin)")
	var (
		dataDirFlag  = fs.String("data-dir", "", "where objects and checkouts live (default ~/.drover)")
		configFlag   = fs.String("config", "", "config file (default <data-dir>/config.yaml)")
		serverFlag   = fs.String("server", "", "engine to talk to")
		noRememberFl = fs.Bool("no-remember", false, "do not add these paths to the config apply list")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("nothing to apply: pass -f <file|dir> (or -f - for stdin)")
	}

	dataDir, err := config.DataDir(*dataDirFlag)
	if err != nil {
		return err
	}
	cfgPath, err := config.Path(*configFlag, dataDir)
	if err != nil {
		return err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	docs, remember, err := collectDocuments(paths)
	if err != nil {
		return err
	}

	// Validate locally first. The server checks everything again -- it does
	// not trust its client -- but failing here means a typo does not need a
	// round trip, and the error names the file the user is looking at.
	if err := validateLocally(docs); err != nil {
		return err
	}

	url, err := cfg.ServerURL(*serverFlag)
	if err != nil {
		return err
	}
	resp, err := client.New(url).Apply(docs)
	if err != nil {
		return err
	}

	for _, warning := range resp.Warnings {
		fmt.Fprintln(os.Stderr, "warning: "+warning)
	}
	for _, r := range resp.Results {
		fmt.Printf("%s/%s %s\n", r.Kind, r.Name, r.Action)
	}

	// Only after the apply succeeded: record where these objects came from, so
	// the next `drover serve` reads the same sources without anyone
	// maintaining the list by hand.
	if !*noRememberFl && len(remember) > 0 {
		changed := false
		for _, p := range remember {
			added, err := cfg.Remember(p)
			if err != nil {
				return err
			}
			changed = changed || added
		}
		if changed {
			if err := cfg.Save(); err != nil {
				return fmt.Errorf("applied, but could not record the path in %s: %w", cfgPath, err)
			}
		}
	}
	return nil
}

// collectDocuments turns -f paths into documents to send, and returns the
// paths worth recording in the config. Stdin has no path, so it is applied
// but not remembered.
func collectDocuments(paths []string) (docs []api.Document, remember []string, err error) {
	for _, p := range paths {
		if p == "-" {
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return nil, nil, fmt.Errorf("read stdin: %w", err)
			}
			docs = append(docs, api.Document{Source: "stdin", Data: string(data)})
			continue
		}
		files, err := config.CollectFiles(p)
		if err != nil {
			return nil, nil, err
		}
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				return nil, nil, err
			}
			docs = append(docs, api.Document{Source: f, Data: string(data)})
		}
		remember = append(remember, p)
	}
	if len(docs) == 0 {
		return nil, nil, fmt.Errorf("nothing to apply")
	}
	return docs, remember, nil
}

// validateLocally runs the same parse and batch rules the server will, so the
// common mistakes are caught before anything is sent.
func validateLocally(docs []api.Document) error {
	batch := object.NewBatch()
	for _, d := range docs {
		objs, err := object.Parse(d.Source, []byte(d.Data))
		if err != nil {
			return err
		}
		if err := batch.AddAll(objs); err != nil {
			return err
		}
	}
	// Only the batch-local rules here. A request may reference an environment
	// applied long ago, which this side cannot see, so cross-object checks
	// belong to the server.
	return batch.CheckLocal()
}
