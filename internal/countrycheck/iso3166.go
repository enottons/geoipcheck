package countrycheck

// ISO 3166-1 alpha-2 assigned codes.
//
// Source: ISO 3166-1, assigned alpha-2 codes.
// Snapshot date: 2026-01-01.
//
// MaxMind GeoLite2 uses one code that is NOT officially assigned: "XK"
// for Kosovo. We accept it. See design doc D12. Refresh this snapshot if
// invalid_country_code errors show up for a code assigned after the date
// above.

// assignedISO is the set of assigned ISO 3166-1 alpha-2 codes plus MaxMind's
// "XK". Membership lookups are O(1).
var assignedISO = func() map[string]struct{} {
	codes := []string{
		"AD", "AE", "AF", "AG", "AI", "AL", "AM", "AO", "AQ", "AR",
		"AS", "AT", "AU", "AW", "AX", "AZ", "BA", "BB", "BD", "BE",
		"BF", "BG", "BH", "BI", "BJ", "BL", "BM", "BN", "BO", "BQ",
		"BR", "BS", "BT", "BV", "BW", "BY", "BZ", "CA", "CC", "CD",
		"CF", "CG", "CH", "CI", "CK", "CL", "CM", "CN", "CO", "CR",
		"CU", "CV", "CW", "CX", "CY", "CZ", "DE", "DJ", "DK", "DM",
		"DO", "DZ", "EC", "EE", "EG", "EH", "ER", "ES", "ET", "FI",
		"FJ", "FK", "FM", "FO", "FR", "GA", "GB", "GD", "GE", "GF",
		"GG", "GH", "GI", "GL", "GM", "GN", "GP", "GQ", "GR", "GS",
		"GT", "GU", "GW", "GY", "HK", "HM", "HN", "HR", "HT", "HU",
		"ID", "IE", "IL", "IM", "IN", "IO", "IQ", "IR", "IS", "IT",
		"JE", "JM", "JO", "JP", "KE", "KG", "KH", "KI", "KM", "KN",
		"KP", "KR", "KW", "KY", "KZ", "LA", "LB", "LC", "LI", "LK",
		"LR", "LS", "LT", "LU", "LV", "LY", "MA", "MC", "MD", "ME",
		"MF", "MG", "MH", "MK", "ML", "MM", "MN", "MO", "MP", "MQ",
		"MR", "MS", "MT", "MU", "MV", "MW", "MX", "MY", "MZ", "NA",
		"NC", "NE", "NF", "NG", "NI", "NL", "NO", "NP", "NR", "NU",
		"NZ", "OM", "PA", "PE", "PF", "PG", "PH", "PK", "PL", "PM",
		"PN", "PR", "PS", "PT", "PW", "PY", "QA", "RE", "RO", "RS",
		"RU", "RW", "SA", "SB", "SC", "SD", "SE", "SG", "SH", "SI",
		"SJ", "SK", "SL", "SM", "SN", "SO", "SR", "SS", "ST", "SV",
		"SX", "SY", "SZ", "TC", "TD", "TF", "TG", "TH", "TJ", "TK",
		"TL", "TM", "TN", "TO", "TR", "TT", "TV", "TW", "TZ", "UA",
		"UG", "UM", "US", "UY", "UZ", "VA", "VC", "VE", "VG", "VI",
		"VN", "VU", "WF", "WS", "YE", "YT", "ZA", "ZM", "ZW",
		// Not ISO-assigned. MaxMind emits XK for Kosovo.
		"XK",
	}
	m := make(map[string]struct{}, len(codes))
	for _, c := range codes {
		m[c] = struct{}{}
	}
	return m
}()

// nearMissHints maps common bad inputs to their likely intended ISO codes.
// Values must themselves exist in assignedISO; the test enforces that.
// Keys are the un-normalized inputs we see in practice; lookup is done on
// the uppercased value, so each key must be uppercase here.
var nearMissHints = map[string]string{
	// United Kingdom. "UK" is the single most common mistake. If we accept
	// it on format and then compare against MaxMind's "GB", every UK user
	// is silently denied.
	"UK":  "GB",
	"GBR": "GB",
	// Alpha-3 codes for a few of the most frequently miswritten countries.
	// Not exhaustive; the goal is to catch common typos, not to translate
	// every alpha-3 code.
	"USA": "US",
	"CAN": "CA",
	"DEU": "DE",
	"FRA": "FR",
	"CHN": "CN",
	"IND": "IN",
	"JPN": "JP",
	"BRA": "BR",
	"AUS": "AU",
	"ITA": "IT",
	"ESP": "ES",
	"RUS": "RU",
	"KOR": "KR",
	"MEX": "MX",
	"NLD": "NL",
	"CHE": "CH",
	"SWE": "SE",
	"NOR": "NO",
	"POL": "PL",
	"TUR": "TR",
	"ZAF": "ZA",
	"ARG": "AR",
	"UKR": "UA",
	"IRL": "IE",
}

// isAssignedCountry reports whether code is an assigned ISO 3166-1 alpha-2
// code or MaxMind's XK. code must already be uppercase.
func isAssignedCountry(code string) bool {
	_, ok := assignedISO[code]
	return ok
}

// hintFor returns a "did you mean" suggestion for code, or empty if there
// is no useful hint. code must already be uppercase.
func hintFor(code string) string {
	return nearMissHints[code]
}
