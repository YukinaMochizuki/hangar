// Package billing is the arithmetic of billing: money in micro-units,
// periods of whole months cut from an anchor, and rating what a tenant held
// at a price list's prices. It reads and stores nothing: the apiserver
// meters, keeps the accounts and makes the statements
// (docs/decisions/billing.md).
package billing

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Micros is an amount of money in millionths of its currency's unit.
type Micros int64

// microDigits is how many digits after the point a price or an amount
// carries.
const microDigits = 6

const perUnit = 1_000_000

// ParseMoney reads a decimal amount, such as "300" or "0.012", at most six
// digits after the point and not negative.
func ParseMoney(s string) (Micros, error) {
	if s == "" {
		return 0, errors.New("empty amount")
	}
	whole, frac, hasPoint := strings.Cut(s, ".")
	if whole == "" || (hasPoint && frac == "") {
		return 0, fmt.Errorf("%q is not a decimal such as 12 or 0.5", s)
	}
	if len(frac) > microDigits {
		return 0, fmt.Errorf("%q has more than %d digits after the point", s, microDigits)
	}
	for _, part := range []string{whole, frac} {
		for _, c := range part {
			if c < '0' || c > '9' {
				return 0, fmt.Errorf("%q is not a decimal such as 12 or 0.5", s)
			}
		}
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w > math.MaxInt64/perUnit-1 {
		return 0, fmt.Errorf("%q is too large", s)
	}
	f := int64(0)
	if frac != "" {
		f, _ = strconv.ParseInt(frac+strings.Repeat("0", microDigits-len(frac)), 10, 64)
	}
	return Micros(w*perUnit + f), nil
}

// String is m as a decimal with no trailing zeros after the point, such as
// "12.5" or "300".
func (m Micros) String() string {
	sign := ""
	v := int64(m)
	if v < 0 {
		sign, v = "-", -v
	}
	whole, frac := v/perUnit, v%perUnit
	if frac == 0 {
		return sign + strconv.FormatInt(whole, 10)
	}
	f := strings.TrimRight(fmt.Sprintf("%06d", frac), "0")
	return sign + strconv.FormatInt(whole, 10) + "." + f
}

// minorDigits are the digits after the point of the currencies whose minor
// unit is not a hundredth (ISO 4217); the rest have two.
var minorDigits = map[string]int{
	"BIF": 0, "CLP": 0, "DJF": 0, "GNF": 0, "ISK": 0, "JPY": 0, "KMF": 0, "KRW": 0,
	"PYG": 0, "RWF": 0, "UGX": 0, "UYI": 0, "VND": 0, "VUV": 0, "XAF": 0, "XOF": 0, "XPF": 0,
	"BHD": 3, "IQD": 3, "JOD": 3, "KWD": 3, "LYD": 3, "OMR": 3, "TND": 3,
}

// ValidCurrency reports whether code is written as an ISO 4217 code is:
// three upper-case letters.
func ValidCurrency(code string) bool {
	if len(code) != 3 {
		return false
	}
	for _, c := range code {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

// RoundToMinor is m rounded half away from zero to currency's minor unit,
// such as a cent.
func RoundToMinor(m Micros, currency string) Micros {
	digits, ok := minorDigits[currency]
	if !ok {
		digits = 2
	}
	step := int64(math.Pow10(microDigits - digits))
	return Micros(divRound(big.NewInt(int64(m)), big.NewInt(step)).Int64() * step)
}

// Share is m times num over den, rounded half up to a micro-unit.
func (m Micros) Share(num, den int64) Micros {
	if den == 0 {
		return 0
	}
	v := new(big.Int).Mul(big.NewInt(int64(m)), big.NewInt(num))
	return Micros(divRound(v, big.NewInt(den)).Int64())
}

// divRound is a over b, b positive, rounded half away from zero.
func divRound(a, b *big.Int) *big.Int {
	q, r := new(big.Int).QuoRem(a, b, new(big.Int))
	if new(big.Int).Mul(new(big.Int).Abs(r), big.NewInt(2)).Cmp(b) >= 0 {
		if a.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	return q
}
