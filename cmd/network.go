package cmd

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/firmfact/cli/internal/httpx"
)

// timeoutFlag is --timeout. It takes a duration (90s, 2m) or a number of
// seconds, and refuses zero and less, which would mean no answer is ever
// waited for.
type timeoutFlag struct{ d *time.Duration }

func (f timeoutFlag) Set(s string) error {
	d, err := parseTimeout(s)
	if err != nil {
		return err
	}
	*f.d = d
	return nil
}

// String is empty until the flag is given, so help shows no default of 0s.
func (f timeoutFlag) String() string {
	if f.d == nil || *f.d == 0 {
		return ""
	}
	return f.d.String()
}

func (f timeoutFlag) Type() string { return "duration" }

func parseTimeout(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var d time.Duration
	if secs, err := strconv.ParseFloat(s, 64); err == nil {
		d = time.Duration(secs * float64(time.Second))
	} else if d, err = time.ParseDuration(s); err != nil {
		return 0, fmt.Errorf("%q is not a duration; give one such as 90s or 2m", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%q: the limit must be longer than zero", s)
	}
	return d, nil
}

// pointAtDoctor makes every command but doctor end the error of a request
// that got no answer with a pointer to doctor, which checks the connection
// step by step. Doctor lists such a failure in its own report.
func pointAtDoctor(cmd, doctor *cobra.Command, name string) {
	for _, sub := range cmd.Commands() {
		pointAtDoctor(sub, doctor, name)
	}
	run := cmd.RunE
	if run == nil || cmd == doctor {
		return
	}
	cmd.RunE = func(c *cobra.Command, args []string) error {
		return withDoctorHint(run(c, args), name)
	}
}

func withDoctorHint(err error, name string) error {
	var netErr *httpx.Error
	if !errors.As(err, &netErr) {
		return err
	}
	hint := "run `" + name + " doctor`"
	if netErr.Kind == httpx.AnswerTimeout {
		hint = "allow longer with --timeout, or " + hint
	}
	return fmt.Errorf("%w; %s", err, hint)
}
