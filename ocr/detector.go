package ocr

import (
	"fmt"
	"math"
	"sort"
)

type pixelBox struct {
	x0, y0, x1, y1 int
	score          float32
}

func detectorInput(image *rgbImage, maxSide int) ([]float32, []int64, float64, float64) {
	ratio := 1.0
	if side := maxInt(image.width, image.height); side > maxSide {
		ratio = float64(maxSide) / float64(side)
	}
	width := maxInt(32, int(math.Round(float64(image.width)*ratio/32))*32)
	height := maxInt(32, int(math.Round(float64(image.height)*ratio/32))*32)
	resized := resizeImage(image, width, height)
	data := make([]float32, 3*width*height)
	mean := [3]float32{0.485, 0.456, 0.406}
	std := [3]float32{0.229, 0.224, 0.225}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			src := (y*width + x) * 3
			// PaddleOCR receives OpenCV BGR input.
			channels := [3]uint8{resized.pixels[src+2], resized.pixels[src+1], resized.pixels[src]}
			for channel := 0; channel < 3; channel++ {
				data[channel*width*height+y*width+x] = (float32(channels[channel])/255 - mean[channel]) / std[channel]
			}
		}
	}
	return data, []int64{1, 3, int64(height), int64(width)},
		float64(image.width) / float64(width), float64(image.height) / float64(height)
}

func detectorBoxes(output tensorOutput, threshold, boxThreshold float32, scaleX, scaleY float64, sourceWidth, sourceHeight int, dilation ...bool) ([]pixelBox, error) {
	if len(output.shape) != 4 || output.shape[0] != 1 || output.shape[1] != 1 {
		return nil, fmt.Errorf("PaddleOCR detector output shape %v, want [1,1,H,W]", output.shape)
	}
	height, width := int(output.shape[2]), int(output.shape[3])
	if height <= 0 || width <= 0 || len(output.data) != width*height {
		return nil, fmt.Errorf("invalid PaddleOCR detector output shape %v with %d values", output.shape, len(output.data))
	}
	probabilities := output.data
	if len(dilation) > 0 && dilation[0] {
		probabilities = dilateProbabilityMap(output.data, width, height, threshold)
	}
	visited := make([]bool, width*height)
	queue := make([]int, 0, 4096)
	boxes := make([]pixelBox, 0, 128)
	for index, probability := range probabilities {
		if probability <= threshold || visited[index] {
			continue
		}
		visited[index] = true
		queue = append(queue[:0], index)
		x0, x1 := index%width, index%width
		y0, y1 := index/width, index/width
		var score float64
		pixels := 0
		for head := 0; head < len(queue); head++ {
			current := queue[head]
			x, y := current%width, current/width
			if x < x0 {
				x0 = x
			}
			if x > x1 {
				x1 = x
			}
			if y < y0 {
				y0 = y
			}
			if y > y1 {
				y1 = y
			}
			// Match DB postprocessing: dilation changes the candidate bitmap,
			// while box confidence is still measured on the original map.
			score += float64(output.data[current])
			pixels++
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					if dx == 0 && dy == 0 {
						continue
					}
					nx, ny := x+dx, y+dy
					if nx < 0 || nx >= width || ny < 0 || ny >= height {
						continue
					}
					next := ny*width + nx
					if !visited[next] && probabilities[next] > threshold {
						visited[next] = true
						queue = append(queue, next)
					}
				}
			}
		}
		componentWidth, componentHeight := x1-x0+1, y1-y0+1
		if componentWidth < 3 || componentHeight < 3 || pixels < 9 {
			continue
		}
		average := float32(score / float64(pixels))
		if average < boxThreshold {
			continue
		}
		area := float64(componentWidth * componentHeight)
		perimeter := float64(2 * (componentWidth + componentHeight))
		expand := int(math.Ceil(area * 1.5 / perimeter))
		box := pixelBox{
			x0:    clampInt(int(math.Floor(float64(x0-expand)*scaleX)), 0, sourceWidth-1),
			y0:    clampInt(int(math.Floor(float64(y0-expand)*scaleY)), 0, sourceHeight-1),
			x1:    clampInt(int(math.Ceil(float64(x1+expand+1)*scaleX)), 1, sourceWidth),
			y1:    clampInt(int(math.Ceil(float64(y1+expand+1)*scaleY)), 1, sourceHeight),
			score: average,
		}
		if box.x1-box.x0 > 3 && box.y1-box.y0 > 3 {
			boxes = append(boxes, box)
		}
	}
	sort.SliceStable(boxes, func(i, j int) bool {
		if boxes[i].y0 == boxes[j].y0 {
			return boxes[i].x0 < boxes[j].x0
		}
		return boxes[i].y0 < boxes[j].y0
	})
	for start := 0; start < len(boxes); {
		end := start + 1
		for end < len(boxes) && boxes[end].y0-boxes[start].y0 < 10 {
			end++
		}
		sort.SliceStable(boxes[start:end], func(i, j int) bool {
			return boxes[start+i].x0 < boxes[start+j].x0
		})
		start = end
	}
	return boxes, nil
}

// dilateProbabilityMap is the pure-Go equivalent of PaddleOCR's optional 2x2
// DB bitmap dilation. Original probabilities are retained for box scoring.
func dilateProbabilityMap(source []float32, width, height int, threshold float32) []float32 {
	result := append([]float32(nil), source...)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			index := y*width + x
			if source[index] <= threshold {
				continue
			}
			for dy := 0; dy <= 1 && y+dy < height; dy++ {
				for dx := 0; dx <= 1 && x+dx < width; dx++ {
					next := (y+dy)*width + x + dx
					if result[next] < source[index] {
						result[next] = source[index]
					}
				}
			}
		}
	}
	return result
}
