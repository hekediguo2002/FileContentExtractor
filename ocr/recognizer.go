package ocr

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"strings"
)

func loadDictionary(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open PaddleOCR dictionary %q: %w", path, err)
	}
	defer file.Close()
	characters := []string{""} // CTC blank.
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	for scanner.Scan() {
		characters = append(characters, strings.TrimSuffix(scanner.Text(), "\r"))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read PaddleOCR dictionary %q: %w", path, err)
	}
	characters = append(characters, " ")
	return characters, nil
}

type recognition struct {
	text       string
	confidence float32
}

func recognizerInput(image *rgbImage) ([]float32, []int64) {
	data, shape := recognizerBatchInput([]*rgbImage{image})
	return data, shape
}

func recognizerBatchInput(images []*rgbImage) ([]float32, []int64) {
	const targetHeight = 48
	maxRatio := 0.0
	for _, image := range images {
		ratio := float64(image.width) / float64(maxInt(1, image.height))
		if ratio > maxRatio {
			maxRatio = ratio
		}
	}
	targetWidth := maxInt(320, int(math.Ceil(targetHeight*maxRatio)))
	if targetWidth > 2048 {
		targetWidth = 2048
	}
	data := make([]float32, len(images)*3*targetHeight*targetWidth)
	imageStride := 3 * targetHeight * targetWidth
	channelStride := targetHeight * targetWidth
	for imageIndex, image := range images {
		ratio := float64(image.width) / float64(maxInt(1, image.height))
		resizedWidth := minInt(targetWidth, maxInt(1, int(math.Ceil(targetHeight*ratio))))
		resized := resizeImage(image, resizedWidth, targetHeight)
		base := imageIndex * imageStride
		for y := 0; y < targetHeight; y++ {
			for x := 0; x < resizedWidth; x++ {
				src := (y*resizedWidth + x) * 3
				channels := [3]uint8{resized.pixels[src+2], resized.pixels[src+1], resized.pixels[src]}
				for channel := 0; channel < 3; channel++ {
					data[base+channel*channelStride+y*targetWidth+x] = float32(channels[channel])/127.5 - 1
				}
			}
		}
	}
	return data, []int64{int64(len(images)), 3, targetHeight, int64(targetWidth)}
}

func decodeRecognition(output tensorOutput, characters []string) (string, float32, error) {
	results, err := decodeRecognitionBatch(output, characters)
	if err != nil {
		return "", 0, err
	}
	if len(results) != 1 {
		return "", 0, fmt.Errorf("PaddleOCR recognizer returned %d results, want 1", len(results))
	}
	return results[0].text, results[0].confidence, nil
}

func decodeRecognitionBatch(output tensorOutput, characters []string) ([]recognition, error) {
	if len(output.shape) != 3 || output.shape[0] <= 0 {
		return nil, fmt.Errorf("PaddleOCR recognizer output shape %v, want [N,T,C]", output.shape)
	}
	batch, steps, classes := int(output.shape[0]), int(output.shape[1]), int(output.shape[2])
	if steps <= 0 || classes <= 1 || len(output.data) != batch*steps*classes {
		return nil, fmt.Errorf("invalid PaddleOCR recognizer output shape %v with %d values", output.shape, len(output.data))
	}
	if classes != len(characters) {
		return nil, fmt.Errorf("PaddleOCR recognizer has %d classes but dictionary provides %d", classes, len(characters))
	}
	results := make([]recognition, batch)
	for batchIndex := 0; batchIndex < batch; batchIndex++ {
		var text strings.Builder
		previous := -1
		var confidence float64
		count := 0
		for step := 0; step < steps; step++ {
			start := (batchIndex*steps + step) * classes
			row := output.data[start : start+classes]
			bestIndex, bestValue := 0, row[0]
			for index, value := range row[1:] {
				if value > bestValue {
					bestIndex, bestValue = index+1, value
				}
			}
			if bestIndex != 0 && bestIndex != previous {
				text.WriteString(characters[bestIndex])
				confidence += float64(bestValue)
				count++
			}
			previous = bestIndex
		}
		if count > 0 {
			results[batchIndex] = recognition{text: text.String(), confidence: float32(confidence / float64(count))}
		}
	}
	return results, nil
}
