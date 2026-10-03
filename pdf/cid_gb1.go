package pdf

import _ "embed"

// adobeGBKEUCData maps the predefined GBK-EUC-H character codes used by
// Adobe-GB1 composite fonts directly to Unicode.
//
//go:embed data/gbk_euc_ucs2.cmap
var adobeGBKEUCData string

var adobeGBKEUCCMap, adobeGBKEUCWidth = parseCMap([]byte(adobeGBKEUCData))
