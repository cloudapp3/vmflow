package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// ByteRate is a normalized byte-per-second value. Configuration accepts common
// network units such as "100 Mbps" and byte-rate units such as "10 MiB/s".
type ByteRate int64

// ByteSize is a normalized byte count. Configuration accepts decimal provider
// units (GB/TB) and IEC units (GiB/TiB).
type ByteSize int64

var quantityPattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([^\s]+)$`)

var byteRateUnits = map[string]*big.Rat{
	"bps":   big.NewRat(1, 8),
	"Kbps":  big.NewRat(1_000, 8),
	"Mbps":  big.NewRat(1_000_000, 8),
	"Gbps":  big.NewRat(1_000_000_000, 8),
	"Tbps":  big.NewRat(1_000_000_000_000, 8),
	"B/s":   big.NewRat(1, 1),
	"KB/s":  big.NewRat(1_000, 1),
	"MB/s":  big.NewRat(1_000_000, 1),
	"GB/s":  big.NewRat(1_000_000_000, 1),
	"TB/s":  big.NewRat(1_000_000_000_000, 1),
	"KiB/s": big.NewRat(1<<10, 1),
	"MiB/s": big.NewRat(1<<20, 1),
	"GiB/s": big.NewRat(1<<30, 1),
	"TiB/s": big.NewRat(1<<40, 1),
}

var byteSizeUnits = map[string]*big.Rat{
	"B":   big.NewRat(1, 1),
	"KB":  big.NewRat(1_000, 1),
	"MB":  big.NewRat(1_000_000, 1),
	"GB":  big.NewRat(1_000_000_000, 1),
	"TB":  big.NewRat(1_000_000_000_000, 1),
	"KiB": big.NewRat(1<<10, 1),
	"MiB": big.NewRat(1<<20, 1),
	"GiB": big.NewRat(1<<30, 1),
	"TiB": big.NewRat(1<<40, 1),
}

func (value *ByteRate) UnmarshalText(text []byte) error {
	parsed, err := parseHumanQuantity(string(text), byteRateUnits, "bandwidth")
	if err != nil {
		return err
	}
	*value = ByteRate(parsed)
	return nil
}

func (value ByteRate) MarshalText() ([]byte, error) {
	return []byte(value.String()), nil
}

func (value *ByteRate) UnmarshalJSON(data []byte) error {
	return unmarshalQuantityJSON(data, func(text []byte) error { return value.UnmarshalText(text) })
}

func (value ByteRate) String() string {
	return formatHumanQuantity(int64(value), []displayUnit{
		{name: "Tbps", factor: big.NewRat(1_000_000_000_000, 8)},
		{name: "Gbps", factor: big.NewRat(1_000_000_000, 8)},
		{name: "Mbps", factor: big.NewRat(1_000_000, 8)},
		{name: "Kbps", factor: big.NewRat(1_000, 8)},
		{name: "TiB/s", factor: big.NewRat(1<<40, 1)},
		{name: "GiB/s", factor: big.NewRat(1<<30, 1)},
		{name: "MiB/s", factor: big.NewRat(1<<20, 1)},
		{name: "KiB/s", factor: big.NewRat(1<<10, 1)},
		{name: "B/s", factor: big.NewRat(1, 1)},
	})
}

func (value *ByteSize) UnmarshalText(text []byte) error {
	parsed, err := parseHumanQuantity(string(text), byteSizeUnits, "traffic quota")
	if err != nil {
		return err
	}
	*value = ByteSize(parsed)
	return nil
}

func (value ByteSize) MarshalText() ([]byte, error) {
	return []byte(value.String()), nil
}

func (value *ByteSize) UnmarshalJSON(data []byte) error {
	return unmarshalQuantityJSON(data, func(text []byte) error { return value.UnmarshalText(text) })
}

func (value ByteSize) String() string {
	return formatHumanQuantity(int64(value), []displayUnit{
		{name: "TB", factor: big.NewRat(1_000_000_000_000, 1)},
		{name: "GB", factor: big.NewRat(1_000_000_000, 1)},
		{name: "MB", factor: big.NewRat(1_000_000, 1)},
		{name: "KB", factor: big.NewRat(1_000, 1)},
		{name: "TiB", factor: big.NewRat(1<<40, 1)},
		{name: "GiB", factor: big.NewRat(1<<30, 1)},
		{name: "MiB", factor: big.NewRat(1<<20, 1)},
		{name: "KiB", factor: big.NewRat(1<<10, 1)},
		{name: "B", factor: big.NewRat(1, 1)},
	})
}

func unmarshalQuantityJSON(data []byte, decode func([]byte) error) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return fmt.Errorf("empty quantity")
	}
	if data[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		return decode([]byte(text))
	}
	var number int64
	if err := json.Unmarshal(data, &number); err != nil {
		return fmt.Errorf("quantity must be a human-readable string or an integer byte value: %w", err)
	}
	return decode([]byte(strconv.FormatInt(number, 10)))
}

func parseHumanQuantity(raw string, units map[string]*big.Rat, kind string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("%s value is empty", kind)
	}
	if raw == "0" {
		return 0, nil
	}
	if integer, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if integer < 0 {
			return 0, fmt.Errorf("%s must be >= 0", kind)
		}
		return integer, nil
	}
	match := quantityPattern.FindStringSubmatch(raw)
	if match == nil {
		return 0, fmt.Errorf("invalid %s %q: include an explicit unit", kind, raw)
	}
	factor, ok := units[match[2]]
	if !ok {
		return 0, fmt.Errorf("invalid %s unit %q", kind, match[2])
	}
	number, ok := new(big.Rat).SetString(match[1])
	if !ok {
		return 0, fmt.Errorf("invalid %s number %q", kind, match[1])
	}
	result := new(big.Rat).Mul(number, factor)
	if !result.IsInt() {
		return 0, fmt.Errorf("%s %q is smaller than one whole byte", kind, raw)
	}
	if !result.Num().IsInt64() || result.Num().Sign() < 0 || result.Num().Cmp(big.NewInt(math.MaxInt64)) > 0 {
		return 0, fmt.Errorf("%s %q exceeds the supported range", kind, raw)
	}
	return result.Num().Int64(), nil
}

type displayUnit struct {
	name   string
	factor *big.Rat
}

func formatHumanQuantity(value int64, units []displayUnit) string {
	if value <= 0 {
		return "0"
	}
	base := big.NewRat(value, 1)
	best := ""
	for _, unit := range units {
		converted := new(big.Rat).Quo(base, unit.factor)
		if converted.Cmp(big.NewRat(1, 1)) < 0 || converted.Cmp(big.NewRat(10_000, 1)) >= 0 || !hasAtMostThreeDecimalPlaces(converted) {
			continue
		}
		candidate := trimDecimal(converted.FloatString(3)) + " " + unit.name
		if best == "" || len(candidate) < len(best) {
			best = candidate
		}
	}
	if best != "" {
		return best
	}
	fallbackUnit := "B"
	if len(units) > 0 {
		fallbackUnit = units[len(units)-1].name
	}
	return strconv.FormatInt(value, 10) + " " + fallbackUnit
}

func hasAtMostThreeDecimalPlaces(value *big.Rat) bool {
	scaled := new(big.Rat).Mul(value, big.NewRat(1_000, 1))
	return scaled.IsInt()
}

func trimDecimal(value string) string {
	value = strings.TrimRight(value, "0")
	return strings.TrimRight(value, ".")
}
