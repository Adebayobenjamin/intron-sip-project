package admission

import "strings"

// Normalise puts a dialled number into the one form the database stores:
// a leading '+' followed by digits.
//
// # Why normalise on write AND on read
//
// Numbers arrive in whatever shape the carrier chose. Twilio sends
// +4930123456; a BYOT SBC may send 004930123456, or 4930123456, or the same
// with spaces in it. A block of numbers entered in one shape and a call
// arriving in another must still meet, or a lookup misses for a number that is
// plainly configured when a human looks at the table.
//
// So every write normalises (see internal/numrange) and every read normalises,
// and the comparison is then meaningful.
//
// # The assumption, stated because it will eventually be wrong
//
// A number with no '+' and no '00' is assumed to already be international.
// That holds for DIDs delivered by a carrier, which is the only source that
// reaches this code — carriers deliver E.164 on the trunks this platform
// buys. It does NOT hold for a national-format number typed into an admin UI,
// which would need a country to resolve against and has nowhere to get one.
// If that ever becomes a source, it needs a per-tenant default country, not a
// guess here.
func Normalise(raw string) string {
	var b strings.Builder
	b.Grow(len(raw) + 1)

	// Keep digits only. Spaces, dashes, parentheses and dots are formatting
	// and carry no information.
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}

	digits := b.String()
	if digits == "" {
		return ""
	}

	// International access prefix. 00 is the ITU form and by far the most
	// common; a carrier using a national prefix such as 011 would need its own
	// handling, and guessing would silently mangle numbers.
	if strings.HasPrefix(digits, "00") {
		digits = digits[2:]
		if digits == "" {
			return ""
		}
	}

	return "+" + digits
}

// Digits returns the number without its leading '+', for the int8range
// comparisons did_ranges uses. A range needs a number, and '+' is not one.
func Digits(normalised string) string {
	return strings.TrimPrefix(normalised, "+")
}
