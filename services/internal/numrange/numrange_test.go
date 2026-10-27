package numrange

import (
	"strings"
	"testing"
)

func TestParse_InclusiveInHalfOpenInStorage(t *testing.T) {
	norm, b, err := Parse(Range{From: "+2348000000100", To: "+2348000000199"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Lower != 2348000000100 || b.Upper != 2348000000200 {
		t.Fatalf("bounds = %+v, want [2348000000100, 2348000000200)", b)
	}
	if norm.From != "+2348000000100" || norm.To != "+2348000000199" {
		t.Fatalf("normalised = %+v", norm)
	}
}

func TestParse_SingleNumberIsARangeOfOne(t *testing.T) {
	_, b, err := Parse(Range{From: "+4930150000", To: "+4930150000"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Upper-b.Lower != 1 {
		t.Fatalf("a single number should span exactly one value, got %+v", b)
	}
}

func TestParse_NormalisesLikeTheCallPath(t *testing.T) {
	norm, _, err := Parse(Range{From: "00234 800 000 0100", To: "+234-800-000-0199"})
	if err != nil {
		t.Fatal(err)
	}
	if norm.From != "+2348000000100" || norm.To != "+2348000000199" {
		t.Fatalf("normalised = %+v", norm)
	}
}

func TestParse_Refuses(t *testing.T) {
	cases := []struct {
		name string
		r    Range
		want string
	}{
		{"reversed", Range{"+2348000000199", "+2348000000100"}, "after"},
		{"different lengths", Range{"+2341000", "+23419999"}, "different lengths"},
		{"leading zero", Range{"+0123", "+0199"}, "starts with 0"},
		{"too long", Range{"+1234567890123456", "+1234567890123457"}, "longer than"},
		{"no digits", Range{"abc", "+123"}, "no digits"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := Parse(c.r)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

func TestParseAll_NamesTheBadEntry(t *testing.T) {
	_, _, err := ParseAll([]Range{
		{"+2348000000100", "+2348000000199"},
		{"+2348000000300", "+2348000000200"},
	})
	if err == nil || !strings.Contains(err.Error(), "did_ranges[1]") {
		t.Fatalf("err = %v, want it to name did_ranges[1]", err)
	}
}

func TestRoundTrip(t *testing.T) {
	in := []Range{{"+2348000000100", "+2348000000199"}, {"+4930150000", "+4930150000"}}
	_, bounds, err := ParseAll(in)
	if err != nil {
		t.Fatal(err)
	}
	lo, hi := Columns(bounds)
	out, err := Zip(lo, hi)
	if err != nil {
		t.Fatal(err)
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("round trip [%d] = %+v, want %+v", i, out[i], in[i])
		}
	}
}

func TestZip_MismatchedLengths(t *testing.T) {
	if _, err := Zip([]int64{1}, nil); err == nil {
		t.Fatal("want an error for mismatched arrays")
	}
}
