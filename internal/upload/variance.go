package upload

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// A threshold is what firmfact upload --fail-on-variance holds an invoice's
// variance preview to, for a pipeline that should stop when an invoice is
// further from its contract than it allows: a percentage of the contracted
// amount (2%), or an amount in the invoice's own currency (50). Both sides
// are exact decimals, never floating point, so an invoice exactly at the
// threshold is within it.

// AnyVariance names the threshold that allows no variance at all, which
// --fail-on-variance takes when it is given no value.
const AnyVariance = "any"

// Threshold is how far an invoice may be from its contract, either way,
// and still pass. The zero Threshold allows no variance: a difference of a
// cent or more exceeds it.
type Threshold struct {
	// Percent is set for a percentage of the contracted amount; otherwise
	// Limit is an amount in the invoice's currency.
	Percent bool
	// Limit is the most the variance may be; nil allows none.
	Limit *big.Rat
	// text is the threshold as it was given, for messages.
	text string
}

// cent is the least difference that counts as a variance. The preview's
// money is rounded to the cent, and firmfact counts less than a cent as
// rounding (Reconciliation::LineComparison::MONEY_EPSILON on the server).
var cent = big.NewRat(1, 100)

// ParseThreshold reads a threshold as a command line gives it: 2%, 2.5%,
// 50, 50.00, or "any" for no variance at all.
func ParseThreshold(s string) (Threshold, error) {
	s = strings.TrimSpace(s)
	if strings.EqualFold(s, AnyVariance) {
		return Threshold{}, nil
	}
	number, percent := strings.CutSuffix(s, "%")
	number = strings.TrimSpace(number)
	switch {
	case number == "":
		return Threshold{}, errors.New("give a percentage of the contracted amount, such as 2%, or an amount in the invoice's currency, such as 50; without a value, any variance counts")
	case strings.Contains(number, ","):
		// 2,5% is two and a half per cent in much of Europe, and 1,000
		// is a thousand in English: neither is guessed at.
		return Threshold{}, fmt.Errorf("%s: write the threshold with a decimal point and without thousands separators, such as 2.5%% or 1000", s)
	}
	limit, ok := Decimal(number).Rat()
	if !ok || limit.Sign() < 0 {
		return Threshold{}, fmt.Errorf("%s is not a threshold: give a percentage of the contracted amount, such as 2%%, or an amount in the invoice's currency, such as 50", s)
	}
	text := number
	if percent {
		text += "%"
	}
	return Threshold{Percent: percent, Limit: limit, text: text}, nil
}

// String is the threshold as it was given, or "any".
func (t Threshold) String() string {
	if t.Limit == nil {
		return AnyVariance
	}
	if t.text != "" {
		return t.text
	}
	// A Threshold made in code rather than parsed.
	text := t.Limit.FloatString(2)
	if t.Percent {
		text += "%"
	}
	return text
}

// Any reports whether t allows no variance at all.
func (t Threshold) Any() bool { return t.Limit == nil || t.Limit.Sign() == 0 }

// Measure is how far an invoice is from its contract, as its variance
// preview says.
type Measure struct {
	// Amount is the invoice side less the contract side, in the invoice's
	// currency: negative below the contract.
	Amount *big.Rat
	// Percent is the size of Amount as a percentage of the contracted
	// amount; nil when there is no contracted amount to take one of.
	Percent *big.Rat
}

// Measure reads how far the invoice is from its contract; ok is false when
// the preview's amount is not a decimal. The percentage is worked out from
// the amount and the contracted amount (the contract side of the matched
// lines, which the server's percent is of too), rather than taken from
// percent, which the server rounds to a tenth: 2.04% must exceed 2%. It
// falls back to percent when the contracted amount is missing.
func (v *Variance) Measure() (m Measure, ok bool) {
	amount, ok := v.Amount.Rat()
	if !ok {
		return Measure{}, false
	}
	m.Amount = amount
	size := new(big.Rat).Abs(amount)
	if contract, ok := v.Contract.Rat(); ok && contract.Sign() > 0 {
		m.Percent = size.Quo(size, contract)
		m.Percent.Mul(m.Percent, big.NewRat(100, 1))
	} else if percent, ok := v.Percent.Rat(); ok && contract == nil {
		m.Percent = percent.Abs(percent)
	}
	return m, true
}

// Exceeded reports whether m is further from the contract than t allows,
// either way. A difference below a cent is rounding, whatever t says. With
// a percentage and no contracted amount to take it of, any difference
// exceeds it: a percentage of nothing allows nothing.
func (t Threshold) Exceeded(m Measure) bool {
	if m.Amount == nil {
		return false
	}
	size := new(big.Rat).Abs(m.Amount)
	switch {
	case size.Cmp(cent) < 0:
		return false
	case t.Limit == nil:
		return true
	case !t.Percent:
		return size.Cmp(t.Limit) > 0
	case m.Percent == nil:
		return true
	}
	return m.Percent.Cmp(t.Limit) > 0
}
