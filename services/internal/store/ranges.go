package store

import "github.com/Adebayobenjamin/intron-sip-project/services/internal/numrange"

// Number ranges cross the driver as two parallel int8 arrays — lower bounds
// and exclusive upper bounds — rather than as multirange values.
//
// pgx can encode multiranges, but arrays keep every query readable in psql,
// keep the Go side free of driver range types, and let range_agg do the one
// genuinely fiddly job: merging adjacent and overlapping blocks into the
// canonical form Postgres compares against. Blocks [100,200) and [200,300)
// written separately come back as one block, [100,300).

// rangeArraysSQL renders a multirange column as its two ordered bound arrays.
func rangeArraysSQL(col string) string {
	return `ARRAY(SELECT lower(r) FROM unnest(` + col + `) AS r ORDER BY lower(r)),
	        ARRAY(SELECT upper(r) FROM unnest(` + col + `) AS r ORDER BY lower(r))`
}

// rangeFromArraysSQL builds a multirange from the bound arrays in the two
// placeholders. An empty pair builds the empty multirange.
func rangeFromArraysSQL(lo, hi string) string {
	return `COALESCE((SELECT range_agg(int8range(l, u))
	                    FROM unnest(` + lo + `::int8[], ` + hi + `::int8[]) AS x(l, u)),
	                 '{}'::int8multirange)`
}

// rangesFrom scans the two arrays back into the API shape.
func rangesFrom(lowers, uppers []int64) ([]numrange.Range, error) {
	out, err := numrange.Zip(lowers, uppers)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []numrange.Range{}
	}
	return out, nil
}
