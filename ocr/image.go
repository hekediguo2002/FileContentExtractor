package ocr

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
)

const maxImagePixels = 200_000_000

type rgbImage struct {
	width, height int
	pixels        []uint8
}

func decodeImage(data []byte) (*rgbImage, error) {
	if len(data) == 0 {
		return nil, errors.New("empty image data")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode image header: %w", err)
	}
	if config.Width <= 0 || config.Height <= 0 ||
		int64(config.Width)*int64(config.Height) > maxImagePixels {
		return nil, fmt.Errorf("unsupported OCR image dimensions %dx%d", config.Width, config.Height)
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	bounds := decoded.Bounds()
	nrgba := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(nrgba, nrgba.Bounds(), decoded, bounds.Min, draw.Src)
	result := &rgbImage{width: bounds.Dx(), height: bounds.Dy(), pixels: make([]uint8, bounds.Dx()*bounds.Dy()*3)}
	for y := 0; y < result.height; y++ {
		for x := 0; x < result.width; x++ {
			src := y*nrgba.Stride + x*4
			dst := (y*result.width + x) * 3
			copy(result.pixels[dst:dst+3], nrgba.Pix[src:src+3])
		}
	}
	return result, nil
}

func resizeImage(src *rgbImage, width, height int) *rgbImage {
	if width == src.width && height == src.height {
		copyPixels := append([]uint8(nil), src.pixels...)
		return &rgbImage{width: width, height: height, pixels: copyPixels}
	}
	dst := &rgbImage{width: width, height: height, pixels: make([]uint8, width*height*3)}
	xScale := float64(src.width) / float64(width)
	yScale := float64(src.height) / float64(height)
	for y := 0; y < height; y++ {
		sy := (float64(y)+0.5)*yScale - 0.5
		y0 := clampInt(int(math.Floor(sy)), 0, src.height-1)
		y1 := clampInt(y0+1, 0, src.height-1)
		fy := sy - math.Floor(sy)
		for x := 0; x < width; x++ {
			sx := (float64(x)+0.5)*xScale - 0.5
			x0 := clampInt(int(math.Floor(sx)), 0, src.width-1)
			x1 := clampInt(x0+1, 0, src.width-1)
			fx := sx - math.Floor(sx)
			for channel := 0; channel < 3; channel++ {
				p00 := float64(src.pixels[(y0*src.width+x0)*3+channel])
				p10 := float64(src.pixels[(y0*src.width+x1)*3+channel])
				p01 := float64(src.pixels[(y1*src.width+x0)*3+channel])
				p11 := float64(src.pixels[(y1*src.width+x1)*3+channel])
				value := (p00*(1-fx)+p10*fx)*(1-fy) + (p01*(1-fx)+p11*fx)*fy
				dst.pixels[(y*width+x)*3+channel] = uint8(clampFloat(value, 0, 255) + 0.5)
			}
		}
	}
	return dst
}

func cropImage(src *rgbImage, box pixelBox) *rgbImage {
	x0 := clampInt(box.x0, 0, src.width-1)
	y0 := clampInt(box.y0, 0, src.height-1)
	x1 := clampInt(box.x1, x0+1, src.width)
	y1 := clampInt(box.y1, y0+1, src.height)
	result := &rgbImage{width: x1 - x0, height: y1 - y0, pixels: make([]uint8, (x1-x0)*(y1-y0)*3)}
	for y := 0; y < result.height; y++ {
		srcStart := ((y0+y)*src.width + x0) * 3
		dstStart := y * result.width * 3
		copy(result.pixels[dstStart:dstStart+result.width*3], src.pixels[srcStart:srcStart+result.width*3])
	}
	if result.height > result.width*3/2 {
		return rotateImage90(result)
	}
	return result
}

func rotateImage90(src *rgbImage) *rgbImage {
	dst := &rgbImage{width: src.height, height: src.width, pixels: make([]uint8, len(src.pixels))}
	for y := 0; y < src.height; y++ {
		for x := 0; x < src.width; x++ {
			dx, dy := y, src.width-1-x
			copy(dst.pixels[(dy*dst.width+dx)*3:(dy*dst.width+dx)*3+3], src.pixels[(y*src.width+x)*3:(y*src.width+x)*3+3])
		}
	}
	return dst
}

func rotateImage180(src *rgbImage) *rgbImage {
	dst := &rgbImage{width: src.width, height: src.height, pixels: make([]uint8, len(src.pixels))}
	pixels := src.width * src.height
	for index := 0; index < pixels; index++ {
		source := index * 3
		target := (pixels - 1 - index) * 3
		copy(dst.pixels[target:target+3], src.pixels[source:source+3])
	}
	return dst
}

func foregroundColor(image *rgbImage) string {
	var histogram [256]int
	candidates := 0
	for index := 0; index+2 < len(image.pixels); index += 3 {
		r, g, b := image.pixels[index], image.pixels[index+1], image.pixels[index+2]
		maximum := maxInt(int(r), maxInt(int(g), int(b)))
		minimum := minInt(int(r), minInt(int(g), int(b)))
		luminance := (299*int(r) + 587*int(g) + 114*int(b)) / 1000
		if luminance < 190 || maximum-minimum > 60 && minimum < 220 {
			histogram[luminance]++
			candidates++
		}
	}
	if candidates == 0 {
		return "#000000"
	}
	target := maxInt(1, candidates/2)
	cutoff, accumulated := 0, 0
	for cutoff < len(histogram) {
		accumulated += histogram[cutoff]
		if accumulated >= target {
			break
		}
		cutoff++
	}
	var red, green, blue uint64
	var count uint64
	for index := 0; index+2 < len(image.pixels); index += 3 {
		r, g, b := image.pixels[index], image.pixels[index+1], image.pixels[index+2]
		maximum := maxInt(int(r), maxInt(int(g), int(b)))
		minimum := minInt(int(r), minInt(int(g), int(b)))
		luminance := (299*int(r) + 587*int(g) + 114*int(b)) / 1000
		if (luminance < 190 || maximum-minimum > 60 && minimum < 220) && luminance <= cutoff {
			red += uint64(r)
			green += uint64(g)
			blue += uint64(b)
			count++
		}
	}
	if count == 0 {
		return "#000000"
	}
	return fmt.Sprintf("#%02X%02X%02X", red/count, green/count, blue/count)
}

func clampInt(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func clampFloat(value, min, max float64) float64 {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
