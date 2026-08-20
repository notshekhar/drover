package main

import "strings"

// splitArgs separates flags from positional arguments.
//
// Go's flag package stops parsing at the first non-flag argument, so
// `drover get repository api -o yaml` would silently ignore -o. People write
// the flag last often enough that dropping it without a word is the wrong
// behaviour, so the arguments are partitioned first and the flag set only
// ever sees flags.
//
// valueFlags names the flags that consume the next argument, which is the one
// thing a generic splitter cannot infer.
func splitArgs(args []string, valueFlags map[string]bool) (flags, positional []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]

		// Everything after "--" is positional by convention.
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			return flags, positional
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = append(positional, a)
			continue
		}

		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue // --flag=value carries its own value
		}
		if valueFlags[name] && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return flags, positional
}

// clientFlags are the value-taking flags shared by the client commands.
func clientFlags(extra ...string) map[string]bool {
	m := map[string]bool{"data-dir": true, "config": true, "server": true}
	for _, e := range extra {
		m[e] = true
	}
	return m
}

// splitArgsAfter is splitArgs, except that once n positional arguments have
// been collected everything remaining is positional too.
//
// This exists for `drover query`, whose statement is arbitrary SQL. A query
// may legitimately begin with "--", which is a SQL comment and not a flag, and
// the generic splitter would hand it to the flag package and fail. Flags for
// these commands go before the statement.
func splitArgsAfter(args []string, valueFlags map[string]bool, n int) (flags, positional []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]

		if len(positional) >= n {
			positional = append(positional, args[i:]...)
			return flags, positional
		}
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			return flags, positional
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = append(positional, a)
			continue
		}

		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue
		}
		if valueFlags[name] && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return flags, positional
}
