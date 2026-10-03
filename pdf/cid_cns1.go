package pdf

import (
	_ "embed"
	"strconv"
	"strings"
)

// adobeCNS1Data is the standard Adobe-CNS1 CID-to-Unicode table. Keeping it
// embedded makes Identity-H decoding deterministic on machines without CJK
// PDF language packs.
//
//go:embed data/adobe_cns1.txt
var adobeCNS1Data string

var adobeCNS1Unicode = parseAdobeCNS1(adobeCNS1Data)

func parseAdobeCNS1(data string) []rune {
	lines := strings.Fields(data)
	result := make([]rune, len(lines))
	for i, line := range lines {
		value, err := strconv.ParseUint(line, 16, 32)
		if err == nil {
			result[i] = rune(value)
		}
	}
	return result
}

func decodeAdobeCNS1(raw []byte) string {
	var result strings.Builder
	for i := 0; i+1 < len(raw); i += 2 {
		cid := int(raw[i])<<8 | int(raw[i+1])
		if cid < len(adobeCNS1Unicode) && adobeCNS1Unicode[cid] != 0 {
			result.WriteRune(adobeCNS1Unicode[cid])
		}
	}
	return result.String()
}
