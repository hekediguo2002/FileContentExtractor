package officeart

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"

	"filecontentextractor/model"
)

func Images(data []byte) []model.Image {
	var result []model.Image
	seen := map[string]bool{}
	for pos := 0; pos+25 <= len(data); pos++ {
		id := binary.LittleEndian.Uint16(data[pos+2 : pos+4])
		format := ""
		switch id {
		case 0xF01D:
			format = "jpg"
		case 0xF01E:
			format = "png"
		case 0xF01F:
			format = "dib"
		case 0xF029:
			format = "tiff"
		default:
			continue
		}
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		start, stop := pos+25, pos+8+size
		if size < 17 || stop > len(data) || start >= stop {
			continue
		}
		raw := append([]byte(nil), data[start:stop]...)
		key := fmt.Sprintf("%x:%d", id, len(raw))
		if seen[key] {
			continue
		}
		seen[key] = true
		im := model.Image{Name: fmt.Sprintf("officeart-%d", pos), Format: format, Data: raw}
		if cfg, _, e := image.DecodeConfig(bytes.NewReader(raw)); e == nil {
			im.Width = cfg.Width
			im.Height = cfg.Height
		}
		result = append(result, im)
		pos = stop - 1
	}
	return result
}
