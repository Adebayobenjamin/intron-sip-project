// Package numrange converts between the two shapes a block of phone numbers
// takes on this platform.
//
//	API        {"from": "+2348000000100", "to": "+2348000000199"}   inclusive, E.164
//	Postgres   int8range [2348000000100, 2348000000200)              half-open, numeric
//
// Numeric, because containment then becomes an index lookup (`@>` on a GiST
// index) rather than a string comparison over every row, and because Postgres
// can refuse an overlap between two blocks by itself — an exclusion constraint
// over ranges — which no amount of application code can do race-free.
//
// Inclusive at the API, because that is how a person writes a block: "100 to
// 199". Half-open in storage, because that is what Postgres normalises every
// discrete range to anyway; converting once here is better than having every
// query remember that upper() is one past the end.
package numrange

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/admission"
)

// maxDigits is E.164's limit. It also keeps every number inside int8, which
// is what makes the numeric form possible at all.
const maxDigits = 15

// Range is an inclusive block of numbers as the API presents it.
type Range struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Bounds are a range in storage form: Lower inclusive, Upper exclusive.
type Bounds struct {
	Lower int64
	Upper int64
}

// Parse validates a range from a request and returns it normalised, with its
// storage bounds.
//
// Both ends must have the SAME NUMBER OF DIGITS. Numerically +2341000 to
// +23419999 is a perfectly good range, and it contains every 8-digit number in
// between as well as the 7-digit ones — a block nobody meant to claim, which
// the overlap constraint would then defend against everyone else. Refusing it
// here is the only place the mistake is visible.
func Parse(r Range) (Range, Bounds, error) {
	from, lo, err := number(r.From)
	if err != nil {
		return Range{}, Bounds{}, fmt.Errorf("from: %w", err)
	}
	to, hi, err := number(r.To)
	if err != nil {
		return Range{}, Bounds{}, fmt.Errorf("to: %w", err)
	}

	if len(from) != len(to) {
		return Range{}, Bounds{}, fmt.Errorf(
			"from %s and to %s have different lengths; a block is numbers of one length", from, to)
	}
	if lo > hi {
		return Range{}, Bounds{}, fmt.Errorf("from %s is after to %s", from, to)
	}

	return Range{From: from, To: to}, Bounds{Lower: lo, Upper: hi + 1}, nil
}

// ParseAll validates a list, naming the offending entry.
func ParseAll(rs []Range) ([]Range, []Bounds, error) {
	norm := make([]Range, 0, len(rs))
	bounds := make([]Bounds, 0, len(rs))
	for i, r := range rs {
		n, b, err := Parse(r)
		if err != nil {
			return nil, nil, fmt.Errorf("did_ranges[%d]: %w", i, err)
		}
		norm = append(norm, n)
		bounds = append(bounds, b)
	}
	return norm, bounds, nil
}

// FromBounds turns storage bounds back into the API shape.
func FromBounds(b Bounds) Range {
	return Range{
		From: "+" + strconv.FormatInt(b.Lower, 10),
		To:   "+" + strconv.FormatInt(b.Upper-1, 10),
	}
}

// Columns splits bounds into the two parallel arrays the store passes to SQL,
// where they are rebuilt with range_agg.
func Columns(bs []Bounds) (lowers, uppers []int64) {
	lowers = make([]int64, 0, len(bs))
	uppers = make([]int64, 0, len(bs))
	for _, b := range bs {
		lowers = append(lowers, b.Lower)
		uppers = append(uppers, b.Upper)
	}
	return lowers, uppers
}

// Zip is the inverse of Columns, for reading the arrays back.
func Zip(lowers, uppers []int64) ([]Range, error) {
	if len(lowers) != len(uppers) {
		return nil, fmt.Errorf("numrange: %d lower bounds but %d upper", len(lowers), len(uppers))
	}
	out := make([]Range, 0, len(lowers))
	for i := range lowers {
		out = append(out, FromBounds(Bounds{Lower: lowers[i], Upper: uppers[i]}))
	}
	return out, nil
}

// Value converts one E.164 number to its storage form, for lookups.
func Value(e164 string) (int64, error) {
	_, n, err := number(e164)
	return n, err
}

// number normalises one end of a range and returns it with its numeric value.
//
// Normalised with the SAME function the call path uses, so a block entered as
// 00234… matches calls that arrive as +234…. A leading zero after the + is
// refused: E.164 has none, and the numeric form would silently drop it — the
// stored block would then be a different block from the one that was asked
// for.
func number(raw string) (string, int64, error) {
	n := admission.Normalise(raw)
	if n == "" {
		return "", 0, fmt.Errorf("%q has no digits", raw)
	}

	digits := strings.TrimPrefix(n, "+")
	switch {
	case len(digits) > maxDigits:
		return "", 0, fmt.Errorf("%s is longer than E.164's %d digits", n, maxDigits)
	case digits[0] == '0':
		return "", 0, fmt.Errorf("%s starts with 0 after the +, which no E.164 number does", n)
	}

	v, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("%s is not a number: %w", n, err)
	}
	return n, v, nil
}
