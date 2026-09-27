package upload

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Schema is the version of the result entries this CLI reads, which the
// server names in each answer's meta.schema. A later version may add
// fields, which the types below ignore and Raw keeps; one that changed a
// field's meaning would have a new number.
const Schema = "document_result/1"

// The states of a document, in document_result/1.
const (
	// In progress: the entry is its state only.
	StateQueued   = "queued"
	StateReading  = "reading"
	StateMatching = "matching"
	StateRetrying = "retrying"
	// Finished: an upload of the caller's own adds what was read.
	StateReadyForReview = "ready_for_review"
	StatePublished      = "published"
	StateAttached       = "attached"
	StateFailed         = "failed"
	StateSkipped        = "skipped"
)

// InProgress reports whether a document in state is still being read, so
// that a poll should ask again. A state this CLI does not know counts as
// finished: a newer server's state then ends the wait and is shown as it
// is, where the other way round the CLI would wait for it until the time
// ran out.
func InProgress(state string) bool {
	switch state {
	case StateQueued, StateReading, StateMatching, StateRetrying:
		return true
	}
	return false
}

// Document is one document in document_result/1. Every entry has ID, State,
// URL and Own. The caller's own uploads add Filename, GroupID and
// CreatedAt, and once finished (not with ?view=state) what was read and
// what needs review. Someone else's document is its state and link only.
//
// The fields are the ones the CLI shows. Raw is the entry as the server
// sent it, unknown fields and all, for --json.
type Document struct {
	ID    string `json:"id"`
	State string `json:"state"`
	URL   string `json:"url"`
	Own   bool   `json:"own"`
	// Reason is over_quota for a document skipped for the monthly
	// allowance.
	Reason    string `json:"reason,omitempty"`
	Filename  string `json:"filename,omitempty"`
	GroupID   string `json:"group_id,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`

	// Type is the document type the intake found, such as invoice,
	// contract or order_form.
	Type string `json:"type,omitempty"`
	// Publishable is whether the review page can publish a document of
	// this type yet.
	Publishable *bool `json:"publishable,omitempty"`
	// Booked is true once the document was published, and for a type that
	// applies itself when it is read (netting guidelines).
	Booked        *bool          `json:"booked,omitempty"`
	Read          *Read          `json:"read,omitempty"`
	ContractMatch *ContractMatch `json:"contract_match,omitempty"`
	Variance      *Variance      `json:"variance,omitempty"`
	Review        *Review        `json:"review,omitempty"`
	Error         *Failure       `json:"error,omitempty"`
	Notes         []string       `json:"notes,omitempty"`

	Raw json.RawMessage `json:"-"`
}

func (d *Document) UnmarshalJSON(b []byte) error {
	type fields Document
	return keepRaw(b, (*fields)(d), &d.Raw)
}

// Read is what the intake read from a document. Amounts are decimal
// strings, dates ISO 8601.
type Read struct {
	Type        string   `json:"type,omitempty"`
	Vendor      *Party   `json:"vendor,omitempty"`
	LegalEntity *Party   `json:"legal_entity,omitempty"`
	Number      string   `json:"number,omitempty"`
	Dates       *Dates   `json:"dates,omitempty"`
	Currency    string   `json:"currency,omitempty"`
	Amounts     *Amounts `json:"amounts,omitempty"`
	Checks      []Check  `json:"checks,omitempty"`
	// Name and Items are a contract's or an order form's title and how
	// many items it lists.
	Name  string `json:"name,omitempty"`
	Items int    `json:"items,omitempty"`
	// Lines are an invoice's.
	Lines []Line `json:"lines,omitempty"`
	// Applied is what netting guidelines changed.
	Applied *Applied `json:"applied,omitempty"`
}

// Party is a vendor or legal entity as read, and how the review links it:
// linked, suggested, new, skipped or unresolved.
type Party struct {
	Name     string `json:"name,omitempty"`
	Status   string `json:"status,omitempty"`
	LinkedTo string `json:"linked_to,omitempty"`
}

// Dates are a document's dates, each ISO 8601 or empty.
type Dates struct {
	Document string `json:"document,omitempty"`
	Due      string `json:"due,omitempty"`
	Start    string `json:"start,omitempty"`
	End      string `json:"end,omitempty"`
}

type Amounts struct {
	Subtotal Decimal `json:"subtotal,omitempty"`
	Tax      Decimal `json:"tax,omitempty"`
	Total    Decimal `json:"total,omitempty"`
}

// Check is one of the intake's checks on what it read, such as whether the
// lines add up to the stated total.
type Check struct {
	Code     string  `json:"code"`
	Status   string  `json:"status"`
	Message  string  `json:"message,omitempty"`
	Stated   Decimal `json:"stated,omitempty"`
	Computed Decimal `json:"computed,omitempty"`
}

// Line is one line of an invoice, and the contract item it is matched to.
type Line struct {
	Number      int        `json:"number"`
	Description string     `json:"description,omitempty"`
	Quantity    Decimal    `json:"quantity,omitempty"`
	UnitPrice   Decimal    `json:"unit_price,omitempty"`
	Amount      Decimal    `json:"amount,omitempty"`
	Currency    string     `json:"currency,omitempty"`
	Period      *Period    `json:"period,omitempty"`
	Item        *ItemMatch `json:"item,omitempty"`
}

// Period is the service period a line bills.
type Period struct {
	Start string `json:"start,omitempty"`
	End   string `json:"end,omitempty"`
}

// ItemMatch is a line's contract item: matched, suggested, new, skipped or
// unmatched. Visible is false for an item of a contract the caller may not
// view, which then has no id or name.
type ItemMatch struct {
	Status     string  `json:"status"`
	ID         string  `json:"id,omitempty"`
	Name       string  `json:"name,omitempty"`
	Visible    *bool   `json:"visible,omitempty"`
	Confidence Decimal `json:"confidence,omitempty"`
}

type Applied struct {
	Listed     int `json:"listed"`
	Delisted   int `json:"delisted"`
	Overridden int `json:"overridden"`
}

// ContractMatch is the contract an invoice is linked or suggested to
// (linked, suggested, new, none), or the one a contract or order form would
// update (existing, new).
type ContractMatch struct {
	Status string `json:"status"`
	// How is automatic or chosen, for a linked contract.
	How           string     `json:"how,omitempty"`
	Score         Decimal    `json:"score,omitempty"`
	RunnerUpScore Decimal    `json:"runner_up_score,omitempty"`
	Contract      *Contract  `json:"contract,omitempty"`
	Suggestions   []Contract `json:"suggestions,omitempty"`
	// Visible is false for a contract the caller may not view.
	Visible *bool  `json:"visible,omitempty"`
	Message string `json:"message,omitempty"`
}

// Contract is a contract in the workspace, with its score when suggested.
type Contract struct {
	ID     string  `json:"id,omitempty"`
	Number string  `json:"number,omitempty"`
	Name   string  `json:"name,omitempty"`
	Start  string  `json:"start,omitempty"`
	End    string  `json:"end,omitempty"`
	Score  Decimal `json:"score,omitempty"`
}

// Variance is a preview of how an invoice compares with its contract, as
// the reconciliation screen would show it once published: variance, none,
// or not_available with a Reason.
type Variance struct {
	Preview  bool            `json:"preview"`
	Status   string          `json:"status"`
	Reason   string          `json:"reason,omitempty"`
	Message  string          `json:"message,omitempty"`
	Currency string          `json:"currency,omitempty"`
	Amount   Decimal         `json:"amount,omitempty"`
	Percent  Decimal         `json:"percent,omitempty"`
	Invoiced Decimal         `json:"invoiced,omitempty"`
	Contract Decimal         `json:"contract,omitempty"`
	Counts   *VarianceCounts `json:"counts,omitempty"`
	Summary  string          `json:"summary,omitempty"`
	Lines    []VarianceLine  `json:"lines,omitempty"`
}

type VarianceCounts struct {
	Lines   int `json:"lines"`
	Matched int `json:"matched"`
	Clean   int `json:"clean"`
}

// VarianceLine is a line that does not match its contract, and why:
// Driver is quantity, unit_price, tax or total.
type VarianceLine struct {
	Line        int      `json:"line"`
	Description string   `json:"description,omitempty"`
	Match       string   `json:"match,omitempty"`
	Status      string   `json:"status,omitempty"`
	Amount      Decimal  `json:"amount,omitempty"`
	Driver      string   `json:"driver,omitempty"`
	Invoiced    Decimal  `json:"invoiced,omitempty"`
	Contract    Decimal  `json:"contract,omitempty"`
	Item        *ItemRef `json:"item,omitempty"`
	Message     string   `json:"message,omitempty"`
}

type ItemRef struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// Review is what the review page will ask of a person, and where it is.
type Review struct {
	URL string `json:"url,omitempty"`
	// AnalysisPending is a review not built yet; the web builds it when
	// the page is opened.
	AnalysisPending bool          `json:"analysis_pending,omitempty"`
	Message         string        `json:"message,omitempty"`
	Counts          *ReviewCounts `json:"counts,omitempty"`
	Items           []ReviewItem  `json:"items,omitempty"`
	MoreItems       int           `json:"more_items,omitempty"`
	Summary         string        `json:"summary,omitempty"`
}

type ReviewCounts struct {
	Total       int `json:"total"`
	Linked      int `json:"linked"`
	Ready       int `json:"ready"`
	Suggested   int `json:"suggested"`
	NeedsReview int `json:"needs_review"`
	Blocked     int `json:"blocked"`
	Updates     int `json:"updates"`
	Skipped     int `json:"skipped"`
}

// ReviewItem is one thing on the review page that needs a person.
type ReviewItem struct {
	Node    string   `json:"node,omitempty"`
	Kind    string   `json:"kind,omitempty"`
	State   string   `json:"state,omitempty"`
	Line    int      `json:"line,omitempty"`
	Name    string   `json:"name,omitempty"`
	Actions []string `json:"actions,omitempty"`
	Message string   `json:"message,omitempty"`
}

// Failure is why a document failed or was skipped, in the service's own
// words.
type Failure struct {
	Code         string `json:"code,omitempty"`
	Message      string `json:"message,omitempty"`
	Attempt      int    `json:"attempt,omitempty"`
	RetryPending bool   `json:"retry_pending,omitempty"`
}

// Decimal is an amount, a quantity or a score as the server writes it: a
// decimal string, so that no digit is lost to floating point. A number is
// taken too, as its digits, and null as empty.
type Decimal string

func (d *Decimal) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case bytes.Equal(b, []byte("null")):
		*d = ""
		return nil
	case len(b) > 0 && b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*d = Decimal(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("a decimal must be a string or a number, not %s", b)
	}
	*d = Decimal(n)
	return nil
}

// keepRaw decodes b into v, and keeps a copy of b in raw.
func keepRaw(b []byte, v any, raw *json.RawMessage) error {
	if err := json.Unmarshal(b, v); err != nil {
		return err
	}
	*raw = bytes.Clone(b)
	return nil
}
