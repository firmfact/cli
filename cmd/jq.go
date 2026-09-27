package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/itchyny/gojq"

	"github.com/firmfact/cli/internal/ui"
)

// jqFilter is --jq: a jq expression that the JSON output goes through, as
// gh's --jq does, for a script that wants a field or two without jq
// installed. gojq runs it inside the CLI.
type jqFilter struct {
	expr string
	code *gojq.Code
	// ctx is the command's, so Ctrl-C stops an expression that runs on
	// (repeat, a long range).
	ctx context.Context
}

// jqFlag is --jq. The expression is compiled as the command line is
// parsed, so a mistake in it stops the command, with exit status 2,
// before anything is sent.
type jqFlag struct{ f **jqFilter }

func (v jqFlag) Set(s string) error {
	expr := strings.TrimSpace(s)
	if expr == "" {
		return errors.New("give a jq expression, such as .data[].name")
	}
	query, err := gojq.Parse(expr)
	if err != nil {
		return fmt.Errorf("not a jq expression: %w", err)
	}
	// Without an environment loader $ENV and env are empty: an expression
	// has no business with FIRMFACT_TOKEN. Without an input iterator,
	// input and inputs fail, as there is one value to filter.
	code, err := gojq.Compile(query)
	if err != nil {
		return fmt.Errorf("not a jq expression: %w", err)
	}
	*v.f = &jqFilter{expr: expr, code: code}
	return nil
}

// String is empty until the flag is given, so help shows no default and
// JSONRequested can tell whether it was.
func (v jqFlag) String() string {
	if v.f == nil || *v.f == nil {
		return ""
	}
	return (*v.f).expr
}

func (v jqFlag) Type() string { return "expression" }

// printFiltered prints what the --jq expression makes of v, a value per
// line, as gh does: a string as its text, as jq -r prints it, and anything
// else as JSON, indented on a terminal and on one line otherwise, so that
// `--jq '.data[]'` gives a script one row per line. Server text is escaped
// as everywhere else (see ui.SafeText and ui.SafeJSON).
func (a *App) printFiltered(v any) error {
	input, err := jqInput(v)
	if err != nil {
		return err
	}
	ctx := a.jq.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	indent := ui.IsTerminal(a.Out)
	iter := a.jq.code.RunWithContext(ctx, input)
	for {
		out, ok := iter.Next()
		if !ok {
			return nil
		}
		if err, isErr := out.(error); isErr {
			var halt *gojq.HaltError
			switch {
			case errors.As(err, &halt) && halt.Value() == nil:
				return nil // halt: stop without an error
			case ctx.Err() != nil:
				return ctx.Err()
			}
			return fmt.Errorf("--jq: %w", err)
		}
		var line []byte
		if s, isString := out.(string); isString {
			line = []byte(ui.SafeText(s))
		} else {
			raw, err := gojq.Marshal(out)
			if err != nil {
				return fmt.Errorf("--jq: %w", err)
			}
			if indent {
				var buf bytes.Buffer
				if json.Indent(&buf, raw, "", "  ") == nil {
					raw = buf.Bytes()
				}
			}
			line = ui.SafeJSON(raw)
		}
		if _, err := a.Out.Write(append(line, '\n')); err != nil {
			return err
		}
	}
}

// jqInput is v as gojq takes it: the JSON that --json would print, decoded
// into maps, slices and plain values. Numbers stay as the JSON wrote them,
// so an id with more digits than a float64 holds comes out as it went in.
func jqInput(v any) (any, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(&buf)
	dec.UseNumber()
	var input any
	if err := dec.Decode(&input); err != nil {
		return nil, err
	}
	return input, nil
}
