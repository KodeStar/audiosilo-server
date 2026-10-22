package discovery

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Provider is a podcast directory a search can ask.
type Provider string

// The providers, in the fixed order GET /podcasts/discovery/providers lists them.
const (
	ITunes       Provider = "itunes"
	PodcastIndex Provider = "podcast_index"
)

// Query bounds and defaults.
const (
	MinQueryRunes  = 2
	MaxQueryRunes  = 200
	MaxQueryBytes  = 800
	DefaultLimit   = 20
	MaxLimit       = 50
	DefaultCountry = "US"
)

// Query is a validated search.
type Query struct {
	Provider Provider
	Text     string // trimmed, Unicode spaces collapsed, case kept
	Limit    int
	Country  string // ISO alpha-2, upper case; "" for Podcast Index
}

// key is the cache and coalescing key: provider, text, country, limit.
func (q Query) key() string {
	return string(q.Provider) + "\x00" + q.Text + "\x00" + q.Country + "\x00" + strconv.Itoa(q.Limit)
}

// ParseQuery validates GET /podcasts/discovery/search's parameters: q (valid
// UTF-8, no control characters, then trimmed with runs of Unicode spaces
// collapsed to one, 2..200 runes and at most 800 bytes), provider (itunes by
// default, or podcast_index), limit (a decimal 1..50, 20 by default) and
// country (a recognized ISO 3166-1 alpha-2 code, US by default, for iTunes
// only). An unknown or repeated parameter is invalid_request.
func ParseQuery(v url.Values) (Query, *Error) {
	for name, vals := range v {
		if len(vals) != 1 || !slices.Contains([]string{"q", "provider", "limit", "country"}, name) {
			return Query{}, errInvalidRequest
		}
	}
	text, ok := cleanQuery(v.Get("q"))
	if !ok || !v.Has("q") {
		return Query{}, errInvalidQuery
	}
	q := Query{Provider: ITunes, Text: text, Limit: DefaultLimit}
	if v.Has("provider") {
		switch p := Provider(v.Get("provider")); p {
		case ITunes, PodcastIndex:
			q.Provider = p
		default:
			return Query{}, errInvalidProvider
		}
	}
	if v.Has("limit") {
		n, ok := decimal(v.Get("limit"))
		if !ok || n < 1 || n > MaxLimit {
			return Query{}, errInvalidLimit
		}
		q.Limit = n
	}
	if q.Provider == PodcastIndex {
		if v.Has("country") {
			return Query{}, errInvalidCountry
		}
		return q, nil
	}
	q.Country = DefaultCountry
	if v.Has("country") {
		c := strings.ToUpper(v.Get("country"))
		if !countries[c] {
			return Query{}, errInvalidCountry
		}
		q.Country = c
	}
	return q, nil
}

// cleanQuery checks and normalizes the search text.
func cleanQuery(s string) (string, bool) {
	if !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl) {
		return "", false
	}
	s = strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
	n := utf8.RuneCountInString(s)
	return s, n >= MinQueryRunes && n <= MaxQueryRunes && len(s) <= MaxQueryBytes
}

// decimal parses a short run of ASCII digits (no sign, no spaces).
func decimal(s string) (int, bool) {
	if s == "" || len(s) > 3 {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}

// countries are the officially assigned ISO 3166-1 alpha-2 codes.
var countries = func() map[string]bool {
	m := map[string]bool{}
	for _, c := range strings.Fields(`AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ
		CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR
		GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP
		KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT
		MU MV MW MX MY MZ NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW
		SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ UA UG
		UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW`) {
		m[c] = true
	}
	return m
}()
