package sigscan

import (
	"math"
	"net/netip"

	"github.com/wlame/rx-go/internal/signatures"
)

// validators are the checks `validate` names, as data: the value of the
// first match must pass the one its signature names.
var validators = map[string]func(value []byte) bool{
	signatures.ValidateLuhn: validLuhn,
	signatures.ValidateIBAN: validIBAN,
	signatures.ValidateIPv4: validIPv4,
	signatures.ValidateIPv6: validIPv6,
}

// validLuhn reports whether value is a number that passes the Luhn
// checksum (card numbers): its digits, with spaces and dashes between
// them skipped, at least two of them; any other byte fails it.
func validLuhn(value []byte) bool {
	sum, digits := 0, 0
	for i := len(value) - 1; i >= 0; i-- {
		c := value[i]
		if c == ' ' || c == '-' {
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
		d := int(c - '0')
		if digits%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		digits++
	}
	return digits >= 2 && sum%10 == 0
}

// ibanLengths is the length of an IBAN in each country that issues
// them, by its two-letter code (the SWIFT IBAN registry).
var ibanLengths = map[string]int{
	"AD": 24, "AE": 23, "AL": 28, "AT": 20, "AZ": 28, "BA": 20, "BE": 16, "BG": 22,
	"BH": 22, "BI": 27, "BR": 29, "BY": 28, "CH": 21, "CR": 22, "CY": 28, "CZ": 24,
	"DE": 22, "DJ": 27, "DK": 18, "DO": 28, "EE": 20, "EG": 29, "ES": 24, "FI": 18,
	"FK": 18, "FO": 18, "FR": 27, "GB": 22, "GE": 22, "GI": 23, "GL": 18, "GR": 27,
	"GT": 28, "HR": 21, "HU": 28, "IE": 22, "IL": 23, "IQ": 23, "IS": 26, "IT": 27,
	"JO": 30, "KW": 30, "KZ": 20, "LB": 28, "LC": 32, "LI": 21, "LT": 20, "LU": 20,
	"LV": 21, "LY": 25, "MC": 27, "MD": 24, "ME": 22, "MK": 19, "MN": 20, "MR": 27,
	"MT": 31, "MU": 30, "NI": 28, "NL": 18, "NO": 15, "OM": 23, "PK": 24, "PL": 28,
	"PS": 29, "PT": 25, "QA": 29, "RO": 24, "RS": 22, "RU": 33, "SA": 24, "SC": 31,
	"SD": 18, "SE": 24, "SI": 19, "SK": 24, "SM": 27, "SO": 23, "ST": 25, "SV": 28,
	"TL": 23, "TN": 24, "TR": 26, "UA": 29, "VA": 22, "VG": 24, "XK": 20, "YE": 30,
}

// maxIBANBytes bounds the bytes validIBAN looks at: the longest IBAN
// (32 characters) written in groups of four with a space between.
const maxIBANBytes = 48

// validIBAN reports whether value is an IBAN: a country code from the
// registry, two check digits, the length that country uses (spaces
// skipped), letters and digits only, and the ISO 13616 checksum: the
// first four characters moved to the end, every letter written as two
// digits (A = 10 … Z = 35), the number modulo 97 is 1. Letters may be
// lower case.
func validIBAN(value []byte) bool {
	if len(value) > maxIBANBytes {
		return false
	}
	chars := make([]byte, 0, len(value))
	for _, c := range value {
		switch {
		case c == ' ':
		case '0' <= c && c <= '9', 'A' <= c && c <= 'Z':
			chars = append(chars, c)
		case 'a' <= c && c <= 'z':
			chars = append(chars, c-'a'+'A')
		default:
			return false
		}
	}
	if len(chars) < 4 || ibanLengths[string(chars[:2])] != len(chars) || !isDigit(chars[2]) || !isDigit(chars[3]) {
		return false
	}
	rotated := make([]byte, 0, len(chars))
	rotated = append(append(rotated, chars[4:]...), chars[:4]...)
	rest := 0
	for _, c := range rotated {
		if c >= 'A' {
			rest = (rest*100 + int(c-'A'+10)) % 97
		} else {
			rest = (rest*10 + int(c-'0')) % 97
		}
	}
	return rest == 1
}

// isDigit reports whether c is an ASCII digit.
func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// validIPv4 reports whether value is an IPv4 address in dotted decimal
// (net/netip rules: four decimal octets, no leading zeros).
func validIPv4(value []byte) bool {
	addr, err := netip.ParseAddr(string(value))
	return err == nil && addr.Is4()
}

// validIPv6 reports whether value is an IPv6 address (net/netip rules,
// an IPv4-mapped form and a zone included).
func validIPv6(value []byte) bool {
	addr, err := netip.ParseAddr(string(value))
	return err == nil && addr.Is6()
}

// shannonEntropy returns the Shannon entropy of value in bits per byte:
// 0 for an empty value or one byte repeated, 8 at most.
func shannonEntropy(value []byte) float64 {
	if len(value) == 0 {
		return 0
	}
	var counts [256]int
	for _, c := range value {
		counts[c]++
	}
	n := float64(len(value))
	h := 0.0
	for _, c := range counts {
		if c > 0 {
			p := float64(c) / n
			h -= p * math.Log2(p)
		}
	}
	return h
}
