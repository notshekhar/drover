package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/notshekhar/drover/internal/api"
	"github.com/notshekhar/drover/internal/client"
	"github.com/notshekhar/drover/internal/config"
	"github.com/notshekhar/drover/internal/object"
)

func clientFor(dataDirFlag, configFlag, serverFlag *string) (*client.Client, error) {
	dataDir, err := config.DataDir(*dataDirFlag)
	if err != nil {
		return nil, err
	}
	cfgPath, err := config.Path(*configFlag, dataDir)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	url, err := cfg.ServerURL(*serverFlag)
	if err != nil {
		return nil, err
	}
	return client.New(url), nil
}

func cmdGet(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		dataDirFlag = fs.String("data-dir", "", "")
		configFlag  = fs.String("config", "", "")
		serverFlag  = fs.String("server", "", "")
		outFlag     = fs.String("o", "table", "output format: table, yaml or json")
	)
	flags, rest := splitArgs(args, clientFlags("o"))
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("which kind? try `drover get repository`")
	}
	kind, err := object.ParseKind(rest[0])
	if err != nil {
		return err
	}

	c, err := clientFor(dataDirFlag, configFlag, serverFlag)
	if err != nil {
		return err
	}

	if len(rest) > 1 {
		v, err := c.Get(ctx, kind, rest[1])
		if err != nil {
			return err
		}
		return printViews([]api.ObjectView{*v}, *outFlag, kind)
	}

	items, err := c.List(ctx, kind)
	if err != nil {
		return err
	}
	return printViews(items, *outFlag, kind)
}

func printViews(items []api.ObjectView, format string, kind object.Kind) error {
	switch format {
	case "yaml":
		for _, v := range items {
			if v.YAML == "" {
				continue
			}
			fmt.Print(v.YAML)
			fmt.Println("---")
		}
		return nil
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(items)
	case "table", "":
		return printTable(items, kind)
	default:
		return fmt.Errorf("unknown output format %q (use table, yaml or json)", format)
	}
}

func printTable(items []api.ObjectView, kind object.Kind) error {
	if len(items) == 0 {
		fmt.Fprintf(os.Stderr, "no %s objects\n", kind)
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	defer w.Flush()

	switch kind {
	case object.KindRepository:
		fmt.Fprintln(w, "NAME\tURL\tBRANCH\tREFRESH\tSTATUS\tCOMMIT")
		for _, v := range items {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				v.Name, v.URL, v.Branch, dash(v.RefreshInterval), status(v), dash(shortCommit(v.Commit)))
		}

	case object.KindEnvironment:
		// Secret values never appear here -- only the variable that backs each
		// one and whether it is actually set.
		fmt.Fprintln(w, "NAME\tVARIABLES\tSECRETS")
		for _, v := range items {
			fmt.Fprintf(w, "%s\t%s\t%s\n", v.Name, dash(strings.Join(v.Variables, ", ")), dash(describeSecrets(v.Secrets)))
		}

	case object.KindHTTPRequest:
		fmt.Fprintln(w, "NAME\tMETHOD\tURL\tENVIRONMENTS\tPARAMS\tAGENTS")
		for _, v := range items {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				v.Name, v.Method, v.URL,
				dash(describeEnvironments(v)),
				dash(strings.Join(v.Params, ", ")),
				offered(v.Safe))
		}

	case object.KindSQLConnection:
		fmt.Fprintln(w, "NAME\tPROVIDER\tACCESS\tMAXROWS\tSTATUS")
		for _, v := range items {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", v.Name, dash(v.Provider), access(v.ReadOnly), v.MaxRows, status(v))
		}

	default:
		fmt.Fprintln(w, "NAME\tAPPLIED\tSOURCE")
		for _, v := range items {
			fmt.Fprintf(w, "%s\t%s\t%s\n", v.Name, dash(v.AppliedAt), dash(v.Source))
		}
	}

	// A failure is easy to miss in a column, so spell it out underneath.
	for _, v := range items {
		if v.Error != "" {
			fmt.Fprintf(os.Stderr, "\n%s: %s\n", v.Name, v.Error)
		}
	}
	return nil
}

// describeSecrets names each secret and its backing variable, and flags the
// ones whose variable is not set -- which is the failure you want to see
// before a call fails, not after.
func describeSecrets(secrets []api.SecretStatus) string {
	var parts []string
	for _, s := range secrets {
		if s.Set {
			parts = append(parts, fmt.Sprintf("%s=$%s", s.Name, s.FromEnv))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=$%s (unset)", s.Name, s.FromEnv))
	}
	return strings.Join(parts, ", ")
}

func describeEnvironments(v api.ObjectView) string {
	var parts []string
	for _, e := range v.Environments {
		if e == v.DefaultEnvironment {
			parts = append(parts, e+"*")
			continue
		}
		parts = append(parts, e)
	}
	return strings.Join(parts, ", ")
}

// offered says whether an agent would be given this request as a tool.
func offered(safe bool) string {
	if safe {
		return "yes"
	}
	return "no (not GET)"
}

func access(readOnly bool) string {
	if readOnly {
		return "read-only"
	}
	return "writes allowed"
}

// status is the phase, with a hint that a failure has detail below.
func status(v api.ObjectView) string {
	if v.Status == "" {
		return "-"
	}
	return v.Status
}

func shortCommit(c string) string {
	if len(c) > 8 {
		return c[:8]
	}
	return c
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func cmdDelete(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		dataDirFlag = fs.String("data-dir", "", "")
		configFlag  = fs.String("config", "", "")
		serverFlag  = fs.String("server", "", "")
	)
	flags, rest := splitArgs(args, clientFlags())
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if len(rest) < 2 {
		return fmt.Errorf("usage: drover delete <kind> <name>")
	}
	kind, err := object.ParseKind(rest[0])
	if err != nil {
		return err
	}

	c, err := clientFor(dataDirFlag, configFlag, serverFlag)
	if err != nil {
		return err
	}
	if err := c.Delete(ctx, kind, rest[1]); err != nil {
		return err
	}
	fmt.Printf("%s/%s deleted\n", kind, rest[1])

	// An apply: path that no longer accounts for any object is dead weight,
	// and on the next serve it would be re-applied and bring the object back.
	return pruneApplyList(ctx, *dataDirFlag, *configFlag, c)
}

// pruneApplyList drops config apply: entries that no stored object still
// points at.
func pruneApplyList(ctx context.Context, dataDirFlag, configFlag string, c *client.Client) error {
	dataDir, err := config.DataDir(dataDirFlag)
	if err != nil {
		return nil // best effort: the delete already succeeded
	}
	cfgPath, err := config.Path(configFlag, dataDir)
	if err != nil {
		return nil
	}
	cfg, err := config.Load(cfgPath)
	if err != nil || len(cfg.Apply) == 0 {
		return nil
	}

	inUse := map[string]bool{}
	for _, kind := range object.Kinds {
		items, err := c.List(ctx, kind)
		if err != nil {
			return nil
		}
		for _, v := range items {
			if v.Source != "" {
				inUse[v.Source] = true
			}
		}
	}

	changed := false
	for _, p := range append([]string(nil), cfg.Apply...) {
		if stillFeeds(p, inUse) {
			continue
		}
		if found, _ := cfg.Forget(p); found {
			changed = true
			fmt.Fprintf(os.Stderr, "dropped %s from apply: in %s (no objects came from it any more)\n", p, cfgPath)
		}
	}
	if changed {
		return cfg.Save()
	}
	return nil
}

// stillFeeds reports whether an apply: path still accounts for a live object,
// either as the exact source file or as the directory one sits in.
func stillFeeds(path string, inUse map[string]bool) bool {
	if inUse[path] {
		return true
	}
	files, err := config.CollectFiles(path)
	if err != nil {
		// The path is gone or unreadable; it feeds nothing.
		return false
	}
	for _, f := range files {
		if inUse[f] {
			return true
		}
	}
	return false
}

func cmdForget(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("forget", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		dataDirFlag = fs.String("data-dir", "", "")
		configFlag  = fs.String("config", "", "")
	)
	flags, rest := splitArgs(args, clientFlags())
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if len(rest) < 1 {
		return fmt.Errorf("usage: drover forget <path>")
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

	found, err := cfg.Forget(rest[0])
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%s is not in apply: in %s", rest[0], cfgPath)
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("dropped %s from %s\n", rest[0], cfgPath)
	return nil
}
