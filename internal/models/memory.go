package models

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
