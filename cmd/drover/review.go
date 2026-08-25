package main

import (
	"context"
	"flag"
	"fmt"
	"os"
)

// cmdReview shows what a repository declared about itself.
//
// There is no `drover trust` that flips a switch. Trust is a line in the
// document you already control -- `spec.trustConfig: true` -- so the decision
// lives where every other piece of desired state lives, and `get -o yaml`
// still prints something you could re-apply. This command is the half that
// was missing: seeing what you would be agreeing to.
func cmdReview(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
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
	if len(rest) == 0 {
		return fmt.Errorf("which repository? try `drover review api`")
	}

	c, err := clientFor(dataDirFlag, configFlag, serverFlag)
	if err != nil {
		return err
	}
	res, err := c.Review(ctx, rest[0])
	if err != nil {
		return err
	}

	switch {
	case res.Error != "":
		fmt.Fprintf(os.Stderr, "%s: %s\n", rest[0], res.Error)
	case res.Summary == "" && res.Documents == "":
		fmt.Fprintf(os.Stderr, "%s declares nothing: no .drover.yaml in the checkout\n", rest[0])
		return nil
	}

	if res.Trusted {
		fmt.Fprintf(os.Stderr, "%s is trusted (spec.trustConfig: true). %s\n", rest[0], res.Summary)
		fmt.Fprintf(os.Stderr, "What it contributed: drover get httprequest -l drover.io/source=repository/%s\n", rest[0])
		return nil
	}

	if res.Documents != "" {
		fmt.Print(res.Documents)
	}
	fmt.Fprintf(os.Stderr, "\nNot applied. A repository's yaml is written by whoever can push to it.\n")
	fmt.Fprintf(os.Stderr, "If you have read the above and want it, add to the Repository document:\n\n")
	fmt.Fprintf(os.Stderr, "  spec:\n    trustConfig: true\n\n")
	return nil
}
