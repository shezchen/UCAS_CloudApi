package biz

import "entgo.io/ent/dialect/sql"

// EffectiveTokensSQL returns the normalized SQL expression used by quota and
// analytics aggregation so every surface applies the same cache-read rule.
func EffectiveTokensSQL(selector *sql.Selector) string {
	return effectiveTokensSQL(selector)
}
