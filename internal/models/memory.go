package models

import (
	"strconv"
	"strings"
)

var MemorySizeMap = map[string]struct{}{
	"KB": {},
	"MB": {},
	"GB": {},
	"TB": {},
}
var MemoryUnitMap = map[string]uint64{
	"KB": 1024,
	"MB": 1024 * 1024,
	"GB": 1024 * 1024 * 1024,
	"TB": 1024 * 1024 * 1024 * 1024,
}

func MemoryGetBytes(size string) (int64, error) {
	unit := size[len(size)-2:]

	valueStr := strings.TrimSuffix(size, unit)
	value, err := strconv.ParseFloat(valueStr, 64)
	if err != nil {
		return 0, err
	}

	return int64(value * float64(MemoryUnitMap[unit])), nil
}
