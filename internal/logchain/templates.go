package logchain

import (
	"regexp"
	"time"
)

// KeyKind says what the key of a part's name is.
type KeyKind int

// The kinds of key a name template gives.
const (
	// KeyNone is the key of the active part, whose name has none.
	KeyNone KeyKind = iota
	// KeyNumber is a rotation number: `syslog.3`, `app.3.log`.
	KeyNumber
	// KeyDate is a date, optionally followed by a number:
	// `syslog-20261001-1790812801`, `app-2026-10-01.3.log`; or a year
	// written where a rotation number goes (`report.2023`, see
	// yearKey).
	KeyDate
)

// Key is the number or date a rotated part's name carries.
//
// It decides the provisional order of a chain (see Group) and which
// numbers are missing. It never decides an answer about the parts'
// text: their timestamps set the real order.
type Key struct {
	// Kind is what the key is; KeyNone for the active part.
	Kind KeyKind
	// Text is the key as the name writes it: "3", "20261001-1790812801",
	// "2026-10-01.3".
	Text string
	// Number is the rotation number of a numbered part, and the number
	// written after the date of a dated part (an epoch suffix, logback's
	// index), 0 when there is none.
	Number int64
	// DateMs is the date of a dated part in milliseconds since the Unix
	// epoch, the name's wall clock read as UTC (an epoch is its own
	// instant); 0 for other parts. It only orders the parts of one
	// chain, so the zone it is read in does not matter.
	DateMs int64
}

// Template is one row of the name-template table: a rotation scheme's
// way of naming a part, and how to get the chain name and the key from
// such a name.
//
// The table is data. A name is tried against the rows in order and the
// first row that matches wins; a name no row matches is no rotated part
// (it may still be the active part of a chain, by being named exactly
// like one).
type Template struct {
	// ID names the row: "numbered", "dated", "dated-ext", "numbered-ext".
	ID string
	// Pattern matches a whole part name. It is compiled once, when the
	// package is initialized. Its named groups are `n` (a number),
	// `date`, `comp` (the compression suffix, ignored for membership)
	// and either `name` or `stem` and `ext`.
	Pattern *regexp.Regexp
	// Name builds the chain name from Pattern's submatches, as
	// regexp.FindStringSubmatch returns them.
	Name func(m []string) string
	// KeyKind is the kind of key the row's names carry.
	KeyKind KeyKind
	// NewestLow says that a lower key is newer, as for logrotate's
	// numbers (`.1` is the newest); otherwise a higher key is newer, as
	// for dates.
	NewestLow bool
	// NeedsActive says that the row's parts count only when the active
	// file exists, so names like `node-1.log` and `node-2.log` alone
	// are no chain.
	NeedsActive bool
}

// The building blocks of the template patterns.
const (
	// anyName is the start of a name: any text, a newline included
	// ((?s) makes `.` match it), so a name is matched as the bytes it
	// is. The `?` makes it as short as possible, so the first number or
	// date that ends the name in the row's shape is the key:
	// `syslog-20261001-1790812801` is `syslog` dated 20261001 with the
	// suffix 1790812801, not `syslog-20261001` dated 1790812801.
	anyName = `(?s:.+?)`
	// separator stands between a name and its key.
	separator = `[._-]`
	// datePattern is a date in one of the shapes rotation tools write:
	// yyyymmdd-HHMMSS (also with T or _), yyyy-mm-dd optionally followed
	// by _ or T and a time of day, yyyymmddhh (or a 10-digit epoch),
	// yyyymmdd. Longer shapes come first so a match takes the whole
	// date.
	datePattern = `\d{8}[-T_]\d{6}` +
		`|\d{4}-\d{2}-\d{2}(?:[_T](?:\d{2}-\d{2}-\d{2}\.\d{3}|\d{2}-\d{2}-\d{2}|\d{6}|\d{2}-\d{2}|\d{4}|\d{2}))?` +
		`|\d{10}` +
		`|\d{8}`
	// extensionPattern is a file extension: a letter and up to seven
	// letters or digits.
	extensionPattern = `[A-Za-z][A-Za-z0-9]{0,7}`
	// compressionSuffix is the compression suffix every row allows at
	// the end of a name.
	compressionSuffix = `(?P<comp>\.(?:gz|bz2|xz|zst))?`
)

// The compiled pattern of each row. They are package variables, so
// Go compiles each one exactly once, before any code of the package
// runs.
var (
	// numberedPattern: `{name}.{N}`, N of 1 to 5 digits (`syslog.1`,
	// `dpkg.log.11.gz`, `dmesg.0`).
	numberedPattern = regexp.MustCompile(`^(?P<name>(?s:.+))\.(?P<n>\d{1,5})` + compressionSuffix + `$`)
	// datedPattern: `{name}{sep}{DATE}`, optionally `{sep}{digits}`
	// (`syslog-20261001-1790812801.gz`, `app.log.2026-10-01_12`,
	// `access_log.1790726400`).
	datedPattern = regexp.MustCompile(`^(?P<name>` + anyName + `)` + separator + `(?P<date>` + datePattern + `)` +
		`(?:` + separator + `(?P<n>\d{1,18}))?` + compressionSuffix + `$`)
	// datedExtPattern: `{stem}{sep}{DATE}`, optionally `{sep}{N}`, then
	// `.{ext}` (`app-2026-10-01.3.log.gz`,
	// `postgresql-2026-10-01_000000.log`).
	datedExtPattern = regexp.MustCompile(`^(?P<stem>` + anyName + `)` + separator + `(?P<date>` + datePattern + `)` +
		`(?:` + separator + `(?P<n>\d{1,5}))?\.(?P<ext>` + extensionPattern + `)` + compressionSuffix + `$`)
	// numberedExtPattern: `{stem}{sep}{N}.{ext}` (`app.1.log.gz`,
	// `app-2.log`).
	numberedExtPattern = regexp.MustCompile(`^(?P<stem>` + anyName + `)` + separator + `(?P<n>\d{1,5})` +
		`\.(?P<ext>` + extensionPattern + `)` + compressionSuffix + `$`)
)

// Templates is the name-template table, tried in this order.
//
// dated-ext comes before numbered-ext: `app-2026-10-01.3.log` also has
// the numbered-ext shape (stem `app-2026-10-01`, number 3), and the
// date is what names it.
var Templates = []Template{
	{ID: "numbered", Pattern: numberedPattern, Name: groupText(numberedPattern, "name"),
		KeyKind: KeyNumber, NewestLow: true},
	{ID: "dated", Pattern: datedPattern, Name: groupText(datedPattern, "name"),
		KeyKind: KeyDate},
	{ID: "dated-ext", Pattern: datedExtPattern, Name: stemDotExt(datedExtPattern),
		KeyKind: KeyDate},
	{ID: "numbered-ext", Pattern: numberedExtPattern, Name: stemDotExt(numberedExtPattern),
		KeyKind: KeyNumber, NewestLow: true, NeedsActive: true},
}

// groupText returns a Name function that takes the chain name from one
// named group of re.
//
// Go note: the function returned is a closure; it keeps the group's
// index, looked up once here, for every later call.
func groupText(re *regexp.Regexp, group string) func(m []string) string {
	i := re.SubexpIndex(group)
	return func(m []string) string { return m[i] }
}

// stemDotExt returns a Name function that builds the chain name from
// the `stem` and `ext` groups of re: `app.1.log` is a part of `app.log`.
func stemDotExt(re *regexp.Regexp) func(m []string) string {
	stem, ext := re.SubexpIndex("stem"), re.SubexpIndex("ext")
	return func(m []string) string { return m[stem] + "." + m[ext] }
}

// nameMatch is what the template table says about one file name.
type nameMatch struct {
	// template is the row that matched.
	template *Template
	// chain is the chain name.
	chain string
	// generation is the name without its compression suffix: the files
	// of one generation in several encodings share it.
	generation string
	// key is the name's key.
	key Key
	// newestLow is the provisional direction of the key: the template's
	// for a key of the template's kind, and a later date is newer for a
	// year a numbered row read (see yearKey).
	newestLow bool
	// beforeNumber and afterNumber are what the generation holds before
	// and after the rotation number of a numbered row (`dpkg.log.`, "";
	// `app.`, `.log`), to name a missing number; empty for other rows.
	beforeNumber, afterNumber string
}

// matchName tries name against the template table and returns what the
// first row that matches says, or false when none does.
//
// The cost is the regexp matches themselves (linear in the name's
// length, as Go's regexp guarantees) and one slice of submatches for
// the row that matches; the key is parsed from the matched bytes
// without further allocation.
func matchName(name string) (nameMatch, bool) {
	for i := range Templates {
		tpl := &Templates[i]
		loc := tpl.Pattern.FindStringSubmatchIndex(name)
		if loc == nil {
			continue
		}
		m := submatches(name, loc)
		chain := tpl.Name(m)
		// SECURITY: a chain name is joined to its directory to make the
		// handle; "." or ".." (from `..1` or `...1` with hidden entries
		// on) would make the handle the directory or its parent.
		if chain == "." || chain == ".." {
			return nameMatch{}, false
		}
		match := nameMatch{template: tpl, chain: chain, generation: name}
		if comp := tpl.Pattern.SubexpIndex("comp"); loc[2*comp] >= 0 {
			match.generation = name[:loc[2*comp]]
		}
		match.key, match.beforeNumber, match.afterNumber = keyOf(tpl, name, loc, match.generation)
		match.newestLow = tpl.NewestLow && match.key.Kind == tpl.KeyKind
		return match, true
	}
	return nameMatch{}, false
}

// submatches turns the index pairs of FindStringSubmatchIndex into the
// submatch strings FindStringSubmatch would give. The strings are
// slices of name, not copies.
func submatches(name string, loc []int) []string {
	m := make([]string, len(loc)/2)
	for i := range m {
		if start := loc[2*i]; start >= 0 {
			m[i] = name[start:loc[2*i+1]]
		}
	}
	return m
}

// keyOf reads the key of a name tpl matched, with loc its submatch
// index pairs, and for a numbered row what the generation holds around
// the number.
func keyOf(tpl *Template, name string, loc []int, generation string) (key Key, before, after string) {
	span := func(group string) (int, int) {
		i := tpl.Pattern.SubexpIndex(group)
		if i < 0 {
			return -1, -1
		}
		return loc[2*i], loc[2*i+1]
	}
	numStart, numEnd := span("n")
	key.Kind = tpl.KeyKind
	if numStart >= 0 {
		key.Number = digitsValue(name[numStart:numEnd])
	}
	if tpl.KeyKind == KeyNumber {
		key.Text = name[numStart:numEnd]
		if year, ok := yearKey(key.Text); ok {
			return year, "", ""
		}
		return key, generation[:numStart], generation[numEnd:]
	}
	dateStart, dateEnd := span("date")
	key.DateMs = dateMillis(name[dateStart:dateEnd])
	keyEnd := dateEnd
	if numStart >= 0 {
		keyEnd = numEnd
	}
	key.Text = name[dateStart:keyEnd]
	return key, "", ""
}

// The numbers yearKey reads as years: four digits, from 1970 to 2100.
const (
	yearDigits = 4
	firstYear  = 1970
	lastYear   = 2100
)

// yearKey reads the digits a numbered row matched (`report.2023`,
// `app.2024.log`) as a year when they are one: four digits from 1970 to
// 2100. Such a file is a yearly part, not the 2023rd rotation of
// `report`: its key is a date, the start of that year in UTC, so a later
// year is newer and no number below it counts as missing. It reports
// false for any other number, which stays a rotation number; 5-digit
// `02024` included.
func yearKey(digits string) (Key, bool) {
	if len(digits) != yearDigits {
		return Key{}, false
	}
	year := digitsValue(digits)
	if year < firstYear || year > lastYear {
		return Key{}, false
	}
	start := time.Date(int(year), time.January, 1, 0, 0, 0, 0, time.UTC)
	return Key{Kind: KeyDate, Text: digits, DateMs: start.UnixMilli()}, true
}

// digitsValue is the value of a run of at most 18 ASCII digits, which
// the patterns guarantee (so it fits an int64).
//
// Go note: the type parameter lets one function read the digits of a
// string and of a byte slice alike, so neither is copied to the other.
func digitsValue[T string | []byte](digits T) int64 {
	var v int64
	for i := 0; i < len(digits); i++ {
		v = v*10 + int64(digits[i]-'0')
	}
	return v
}

// dateMillis reads a date the date pattern matched as milliseconds
// since the Unix epoch.
//
// Only its digits count: the separators are fixed by the pattern, so
// the number of digits says the shape (8 yyyymmdd, 10 yyyymmddhh, 12
// with minutes, 14 with seconds, 17 with milliseconds). A run of digits
// that is no valid calendar date, such as a 10-digit epoch, is read as
// epoch seconds.
func dateMillis(date string) int64 {
	// A fixed array on the stack holds the digits: the longest date
	// shape has 17.
	var digits [24]byte
	n := 0
	for i := 0; i < len(date) && n < len(digits); i++ {
		if c := date[i]; c >= '0' && c <= '9' {
			digits[n] = c
			n++
		}
	}
	if ms, ok := calendarMillis(digits[:n]); ok {
		return ms
	}
	return digitsValue(digits[:n]) * 1000
}

// calendarFields is the length of each date-time field in the digits
// of a date, in order: year, month, day, hour, minute, second,
// millisecond.
var calendarFields = [...]int{4, 2, 2, 2, 2, 2, 3}

// calendarDigitCounts are the digit counts a calendar date may have:
// how many of calendarFields each one fills.
var calendarDigitCounts = map[int]int{8: 3, 10: 4, 12: 5, 14: 6, 17: 7}

// calendarMillis reads digits as a calendar date and time in UTC, and
// reports false when the count of digits is no date shape or a field is
// out of its range (a month 13, a 30 February, an hour 24).
func calendarMillis(digits []byte) (int64, bool) {
	fields, ok := calendarDigitCounts[len(digits)]
	if !ok {
		return 0, false
	}
	// values: year, month, day, hour, minute, second, millisecond.
	values := [len(calendarFields)]int{0, 1, 1, 0, 0, 0, 0}
	at := 0
	for f := 0; f < fields; f++ {
		values[f] = int(digitsValue(digits[at : at+calendarFields[f]]))
		at += calendarFields[f]
	}
	year, month, day, hour, minute, second, milli := values[0], values[1], values[2], values[3], values[4], values[5], values[6]
	if month < 1 || month > 12 || hour > 23 || minute > 59 || second > 59 {
		return 0, false
	}
	t := time.Date(year, time.Month(month), day, hour, minute, second, milli*int(time.Millisecond), time.UTC)
	// time.Date carries an overflow into the next field (30 February
	// becomes 2 March); a day that does not come back unchanged was not
	// a date.
	if t.Day() != day {
		return 0, false
	}
	return t.UnixMilli(), true
}
