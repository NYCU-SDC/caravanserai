// Package quantity parses and formats the Kubernetes-style resource quantity
// strings carried in v1.ResourceList, e.g. cpu "500m" and memory "512Mi".
//
// It deliberately covers only what cara uses — CPU in millicores and memory in
// bytes — rather than the full Kubernetes quantity grammar.
package quantity

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ErrInvalid is returned for a string that is not a valid quantity.
var ErrInvalid = errors.New("invalid quantity")

// memorySuffixes maps each accepted memory suffix to its multiplier. Binary
// suffixes are listed before decimal ones so "Mi" is matched before "M".
var memorySuffixes = []struct {
	suffix     string
	multiplier int64
}{
	{"Ki", 1 << 10},
	{"Mi", 1 << 20},
	{"Gi", 1 << 30},
	{"Ti", 1 << 40},
	{"k", 1e3},
	{"K", 1e3},
	{"M", 1e6},
	{"G", 1e9},
	{"T", 1e12},
}

// binaryUnits is the formatting order for memory, largest unit first.
var binaryUnits = []struct {
	suffix string
	size   int64
}{
	{"Ti", 1 << 40},
	{"Gi", 1 << 30},
	{"Mi", 1 << 20},
	{"Ki", 1 << 10},
}

// ParseCPU parses a CPU quantity into millicores. It accepts a millicore
// value ("500m") or a whole or fractional core count ("2", "0.5").
func ParseCPU(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if milli, ok := strings.CutSuffix(s, "m"); ok {
		n, err := strconv.ParseInt(milli, 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%w: cpu %q", ErrInvalid, s)
		}
		return n, nil
	}
	cores, err := strconv.ParseFloat(s, 64)
	if err != nil || cores < 0 || math.IsInf(cores, 0) || math.IsNaN(cores) || cores > math.MaxInt64/1000 {
		return 0, fmt.Errorf("%w: cpu %q", ErrInvalid, s)
	}
	return int64(math.Round(cores * 1000)), nil
}

// ParseMemory parses a memory quantity into bytes. It accepts a plain byte
// count ("1024"), a binary suffix ("512Mi", "4Gi") or a decimal suffix
// ("500M", "1G").
func ParseMemory(s string) (int64, error) {
	s = strings.TrimSpace(s)
	number, multiplier := s, int64(1)
	for _, u := range memorySuffixes {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			number, multiplier = n, u.multiplier
			break
		}
	}
	n, err := strconv.ParseInt(number, 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("%w: memory %q", ErrInvalid, s)
	}
	return n * multiplier, nil
}

// FormatCPU formats millicores as a millicore quantity, e.g. 2000 → "2000m".
func FormatCPU(milli int64) string {
	return strconv.FormatInt(milli, 10) + "m"
}

// FormatMemory formats bytes using the largest binary unit that divides the
// value exactly, e.g. 4294967296 → "4Gi". A value no unit divides is written
// as a plain byte count.
func FormatMemory(bytes int64) string {
	if bytes != 0 {
		for _, u := range binaryUnits {
			if bytes%u.size == 0 {
				return strconv.FormatInt(bytes/u.size, 10) + u.suffix
			}
		}
	}
	return strconv.FormatInt(bytes, 10)
}
