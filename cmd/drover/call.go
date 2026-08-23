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
)

// paramFlag collects repeated -p name=value pairs.
type paramFlag map[string]string

func (p paramFlag) String() string { return fmt.Sprint(map[string]string(p)) }

func (p paramFlag) Set(v string) error {
	name, value, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("expected name=value, got %q", v)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("parameter name is empty in %q", v)
	}
	if _, dup := p[name]; dup {
		return fmt.Errorf("parameter %q given twice", name)
	}
	p[name] = value
	return nil
}

func cmdCall(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("call", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	params := paramFlag{}
	fs.Var(params, "p", "parameter as name=value (repeatable)")
	var (
		dataDirFlag = fs.String("data-dir", "", "")
		configFlag  = fs.String("config", "", "")
		serverFlag  = fs.String("server", "", "")
		envFlag     = fs.String("environment", "", "which environment to run against")
		outFlag     = fs.String("o", "body", "output: body, json or head")
		unsafeFlag  = fs.Bool("allow-write", false, "permit a non-GET request (never available to agents)")
	)
	flags, rest := splitArgs(args, clientFlags("p", "environment", "o"))
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if len(rest) < 1 {
		return fmt.Errorf("usage: drover call <httprequest-name> [-p name=value] [--environment prod]")
	}

	c, err := clientFor(dataDirFlag, configFlag, serverFlag)
	if err != nil {
		return err
	}

	resp, err := c.Call(ctx, rest[0], api.CallRequest{
		Environment:       *envFlag,
		Params:            params,
		AllowUnsafeMethod: *unsafeFlag,
	})
	if err != nil {
		return err
	}

	switch *outFlag {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	case "head":
		fmt.Printf("%s %s -> %d (%dms)\n", resp.Method, resp.URL, resp.Status, resp.DurationMS)
		for k, v := range resp.Headers {
			fmt.Printf("%s: %s\n", k, v)
		}
		return nil
	case "body", "":
		// Status goes to stderr so the body can be piped into jq without
		// stripping a header line first.
		fmt.Fprintf(os.Stderr, "%s %s -> %d (%dms)\n", resp.Method, resp.URL, resp.Status, resp.DurationMS)
		fmt.Println(resp.Body)
		if resp.Truncated {
			fmt.Fprintln(os.Stderr, "(response truncated)")
		}
		if resp.Status >= 400 {
			return fmt.Errorf("remote returned %d", resp.Status)
		}
		return nil
	default:
		return fmt.Errorf("unknown output format %q (use body, json or head)", *outFlag)
	}
}

func cmdQuery(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		dataDirFlag = fs.String("data-dir", "", "")
		configFlag  = fs.String("config", "", "")
		serverFlag  = fs.String("server", "", "")
		outFlag     = fs.String("o", "table", "output: table, json or csv")
	)
	// Everything after the connection name is the statement, verbatim: SQL can
	// start with "--", which is a comment rather than a flag.
	flags, rest := splitArgsAfter(args, clientFlags("o"), 1)
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if len(rest) < 2 {
		return fmt.Errorf("usage: drover query <sqlconnection-name> \"SELECT ...\" (flags go before the statement)")
	}

	c, err := clientFor(dataDirFlag, configFlag, serverFlag)
	if err != nil {
		return err
	}
	res, err := c.Query(ctx, rest[0], strings.Join(rest[1:], " "))
	if err != nil {
		return err
	}

	switch *outFlag {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	case "csv":
		fmt.Println(strings.Join(res.Columns, ","))
		for _, row := range res.Rows {
			fmt.Println(strings.Join(row, ","))
		}
	case "table", "":
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(w, strings.Join(res.Columns, "\t"))
		for _, row := range res.Rows {
			fmt.Fprintln(w, strings.Join(row, "\t"))
		}
		w.Flush()
	default:
		return fmt.Errorf("unknown output format %q (use table, json or csv)", *outFlag)
	}

	note := fmt.Sprintf("%d row(s) in %dms on %s", res.RowCount, res.ElapsedMS, res.Provider)
	if res.Truncated {
		note += " (truncated -- raise spec.maxRows to see more)"
	}
	fmt.Fprintln(os.Stderr, note)
	return nil
}

func cmdHealth(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
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
	if len(rest) < 1 {
		return fmt.Errorf("usage: drover health <sqlconnection-name>")
	}

	c, err := clientFor(dataDirFlag, configFlag, serverFlag)
	if err != nil {
		return err
	}
	if err := c.Health(ctx, rest[0]); err != nil {
		return err
	}
	fmt.Printf("%s is healthy; a sql tool will be offered for it\n", rest[0])
	return nil
}
