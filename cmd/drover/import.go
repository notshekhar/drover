package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/notshekhar/drover/internal/api"
	"github.com/notshekhar/drover/internal/importer"
)

// cmdImport turns a collection somebody already has into drover documents.
//
// It writes yaml to stdout by default rather than applying. What comes out is
// an ordinary document: reviewable, editable, committable. An importer that
// applied straight into the engine would make its guesses invisible, and an
// importer's guesses are the part worth looking at.
func cmdImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		fileFlag    = fs.String("f", "", "the spec file, or the collection directory")
		prefixFlag  = fs.String("prefix", "", "prepend this to every generated name")
		envFlag     = fs.String("environment", "", "name of the Environment to generate (OpenAPI only)")
		tagFlag     = fs.String("tag", "", "only operations carrying one of these tags (comma-separated)")
		allFlag     = fs.Bool("all", false, "import a large collection in full")
		outFlag     = fs.String("out", "-", "write here instead of stdout")
		dataDirFlag = fs.String("data-dir", "", "")
		configFlag  = fs.String("config", "", "")
		serverFlag  = fs.String("server", "", "")
		applyFlag   = fs.Bool("apply", false, "apply the result instead of printing it")
	)
	flags, rest := splitArgs(args, clientFlags("f", "prefix", "environment", "tag", "out"))
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("which format? try `drover import openapi -f openapi.yaml` or `drover import bruno -f ./collection`")
	}
	source := *fileFlag
	if source == "" && len(rest) > 1 {
		source = rest[1]
	}
	if source == "" {
		return fmt.Errorf("import needs -f <file|dir>")
	}

	opts := importer.Options{
		Prefix:      *prefixFlag,
		Environment: *envFlag,
		All:         *allFlag,
	}
	if *tagFlag != "" {
		for _, t := range strings.Split(*tagFlag, ",") {
			if t = strings.TrimSpace(t); t != "" {
				opts.Tags = append(opts.Tags, t)
			}
		}
	}

	var (
		res *importer.Result
		err error
	)
	switch strings.ToLower(rest[0]) {
	case "openapi", "swagger", "oas":
		data, readErr := os.ReadFile(source)
		if readErr != nil {
			return readErr
		}
		res, err = importer.OpenAPI(data, opts)
	case "bruno", "bru":
		res, err = importer.Bruno(source, opts)
	default:
		return fmt.Errorf("unknown format %q (openapi or bruno)", rest[0])
	}
	if err != nil {
		return err
	}

	// A large collection announces itself instead of producing four hundred
	// documents nobody asked for. The fix is one flag away, and the message
	// says which one.
	if res.Truncated {
		return fmt.Errorf("%s has %d operations, which is more than %d.\n"+
			"Narrow it with --tag <tag>, or pass --all if you meant all of them",
			describeSource(source, res.Title), res.Requests, importer.Threshold)
	}

	for _, s := range res.Skipped {
		fmt.Fprintf(os.Stderr, "skipped %s\n", s)
	}

	if *applyFlag {
		c, err := clientFor(dataDirFlag, configFlag, serverFlag)
		if err != nil {
			return err
		}
		resp, err := c.Apply(ctx, []api.Document{{Source: source, Data: string(res.Documents)}}, false)
		if err != nil {
			return err
		}
		_ = resp
		fmt.Fprintf(os.Stderr, "applied %d request(s) from %s\n", res.Requests, describeSource(source, res.Title))
		return nil
	}

	if *outFlag == "-" {
		_, err := os.Stdout.Write(res.Documents)
		fmt.Fprintf(os.Stderr, "\n%d request(s) from %s -- review it, then apply it\n", res.Requests, describeSource(source, res.Title))
		return err
	}
	if err := os.WriteFile(*outFlag, res.Documents, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %d request(s) to %s -- review it, then `drover apply -f %s`\n", res.Requests, *outFlag, *outFlag)
	return nil
}

func describeSource(path, title string) string {
	if strings.TrimSpace(title) != "" {
		return title
	}
	return filepath.Base(path)
}
