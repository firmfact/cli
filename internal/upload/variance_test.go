package upload

import (
	"strings"
	"testing"
)

// A threshold is a percentage or an amount, exactly as typed; "any" and
// zero allow no variance. What cannot be read for certain is refused
// rather than guessed at: a decimal comma, a thousands separator, a sign,
// an exponent, words.
func TestParseThreshold(t *testing.T) {
	for _, c := range []struct {
		in      string
		percent bool
		limit   string // the limit as a fraction, "" for none
		text    string
	}{
		{"2%", true, "2/1", "2%"},
		{" 2.5 % ", true, "5/2", "2.5%"},
		{"0.25%", true, "1/4", "0.25%"},
		{"150%", true, "150/1", "150%"},
		{"50", false, "50/1", "50"},
		{"50.00", false, "50/1", "50.00"},
		{"0", false, "0/1", "0"},
		{"any", false, "", "any"},
		{"ANY", false, "", "any"},
	} {
		got, err := ParseThreshold(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		limit := ""
		if got.Limit != nil {
			limit = got.Limit.String()
		}
		if got.Percent != c.percent || limit != c.limit || got.String() != c.text {
			t.Errorf("%q = percent %v, limit %q, %q; want %v, %q, %q", c.in, got.Percent, limit, got.String(), c.percent, c.limit, c.text)
		}
	}
	for _, c := range []struct{ in, want string }{
		{"", "give a percentage of the contracted amount, such as 2%"},
		{"%", "give a percentage"},
		{"2,5%", "2,5%: write the threshold with a decimal point and without thousands separators"},
		{"1,000", "without thousands separators"},
		{"-2%", "-2% is not a threshold"},
		{"1e3", "1e3 is not a threshold"},
		{"two", "two is not a threshold"},
		{"2%%", "2%% is not a threshold"},
		{"EUR 50", "EUR 50 is not a threshold"},
	} {
		if _, err := ParseThreshold(c.in); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v, want %q", c.in, err, c.want)
		}
	}
}

func mustThreshold(t *testing.T, s string) Threshold {
	t.Helper()
	th, err := ParseThreshold(s)
	if err != nil {
		t.Fatal(err)
	}
	return th
}

// A threshold is exceeded only past it, either way from the contract: at
// it is within. A percentage is of the contracted amount and exact, not
// the server's percent rounded to a tenth. Less than a cent is rounding,
// whatever the threshold.
func TestThresholdExceeded(t *testing.T) {
	// 1,000.00 contracted: 20.00 is exactly 2%, 20.40 is 2.04%, which the
	// server rounds to 2.0.
	preview := func(amount, percent string) *Variance {
		return &Variance{Status: "variance", Amount: Decimal(amount), Percent: Decimal(percent), Contract: "1000.00"}
	}
	for _, c := range []struct {
		threshold string
		v         *Variance
		want      bool
	}{
		{"2%", preview("19.99", "2.0"), false},
		{"2%", preview("20.00", "2.0"), false},
		{"2%", preview("20.40", "2.0"), true},
		{"2%", preview("-20.40", "2.0"), true},
		{"2%", preview("-20.00", "2.0"), false},
		{"50", preview("49.99", "5.0"), false},
		{"50", preview("50.00", "5.0"), false},
		{"50", preview("50.01", "5.0"), true},
		{"50", preview("-50.01", "5.0"), true},
		{"any", preview("0.01", "0.0"), true},
		{"any", preview("-0.01", "0.0"), true},
		// Lines that differ but add up to the contract.
		{"any", preview("0.00", "0.0"), false},
		{"0%", preview("0.00", "0.0"), false},
		{"0", preview("0.01", "0.0"), true},
		// Past the cent the server rounds to: still rounding.
		{"any", preview("0.004", ""), false},
		// No contracted amount: a percentage of nothing allows nothing,
		// while an amount still counts.
		{"200%", &Variance{Amount: "30.00", Contract: "0.00"}, true},
		{"50", &Variance{Amount: "30.00", Contract: "0.00"}, false},
		// No contracted amount sent: the server's percent stands in.
		{"2%", &Variance{Amount: "30.00", Percent: "2.6"}, true},
		{"3%", &Variance{Amount: "30.00", Percent: "2.6"}, false},
		{"3%", &Variance{Amount: "30.00"}, true},
	} {
		m, ok := c.v.Measure()
		if !ok {
			t.Fatalf("%+v: not measured", c.v)
		}
		if got := mustThreshold(t, c.threshold).Exceeded(m); got != c.want {
			t.Errorf("%s against %s (of %q): exceeded %v, want %v", c.threshold, c.v.Amount, c.v.Contract, got, c.want)
		}
	}
}

// An amount that is not a decimal cannot be measured, and a Measure
// without one exceeds nothing.
func TestMeasureNeedsAnAmount(t *testing.T) {
	for _, amount := range []Decimal{"", "1e3", "EUR 30.00"} {
		if _, ok := (&Variance{Amount: amount, Contract: "100.00"}).Measure(); ok {
			t.Errorf("%q was measured", amount)
		}
	}
	if (Threshold{}).Exceeded(Measure{}) {
		t.Error("an empty Measure exceeded")
	}
	m, _ := (&Variance{Amount: "-14.20", Contract: "100.00"}).Measure()
	if m.Amount.FloatString(2) != "-14.20" || m.Percent.FloatString(1) != "14.2" {
		t.Errorf("measure = %s, %s%%", m.Amount.FloatString(2), m.Percent.FloatString(1))
	}
}

// A threshold made in code names itself too, and "any" and zero both
// allow nothing.
func TestThresholdString(t *testing.T) {
	th := mustThreshold(t, "2%")
	th.text = ""
	if th.String() != "2.00%" {
		t.Errorf("String() = %q", th.String())
	}
	for _, s := range []string{"any", "0", "0%", "0.00"} {
		if !mustThreshold(t, s).Any() {
			t.Errorf("%s allows a variance", s)
		}
	}
	if mustThreshold(t, "0.01").Any() {
		t.Error("0.01 allows none")
	}
}
