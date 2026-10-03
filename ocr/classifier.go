package ocr

import (
	"fmt"
	"math"
)

const (
	classifierHeight = 48
	classifierWidth  = 192
)

func classifyOrientations(session inferenceSession, images []*rgbImage, batchSize int, threshold float32) error {
	batchSize = maxInt(1, batchSize)
	for start := 0; start < len(images); start += batchSize {
		end := minInt(len(images), start+batchSize)
		input, shape := classifierBatchInput(images[start:end])
		output, err := session.run(shape, input)
		if err != nil {
			return err
		}
		rotate, err := decodeOrientationBatch(output, threshold)
		if err != nil {
			return err
		}
		for index, shouldRotate := range rotate {
			if shouldRotate {
				images[start+index] = rotateImage180(images[start+index])
			}
		}
	}
	return nil
}

func classifierBatchInput(images []*rgbImage) ([]float32, []int64) {
	imageStride := 3 * classifierHeight * classifierWidth
	channelStride := classifierHeight * classifierWidth
	data := make([]float32, len(images)*imageStride)
	for imageIndex, image := range images {
		ratio := float64(image.width) / float64(maxInt(1, image.height))
		width := minInt(classifierWidth, maxInt(1, int(math.Ceil(classifierHeight*ratio))))
		resized := resizeImage(image, width, classifierHeight)
		base := imageIndex * imageStride
		for y := 0; y < classifierHeight; y++ {
			for x := 0; x < width; x++ {
				source := (y*width + x) * 3
				channels := [3]uint8{resized.pixels[source+2], resized.pixels[source+1], resized.pixels[source]}
				for channel := 0; channel < 3; channel++ {
					data[base+channel*channelStride+y*classifierWidth+x] = float32(channels[channel])/127.5 - 1
				}
			}
		}
	}
	return data, []int64{int64(len(images)), 3, classifierHeight, classifierWidth}
}

func decodeOrientationBatch(output tensorOutput, threshold float32) ([]bool, error) {
	if len(output.shape) != 2 || output.shape[0] <= 0 || output.shape[1] != 2 {
		return nil, fmt.Errorf("PaddleOCR classifier output shape %v, want [N,2]", output.shape)
	}
	batch := int(output.shape[0])
	if len(output.data) != batch*2 {
		return nil, fmt.Errorf("invalid PaddleOCR classifier output shape %v with %d values", output.shape, len(output.data))
	}
	rotate := make([]bool, batch)
	for index := 0; index < batch; index++ {
		zero, one := output.data[index*2], output.data[index*2+1]
		confidence := one
		if zero < 0 || zero > 1 || one < 0 || one > 1 || math.Abs(float64(zero+one-1)) > 0.01 {
			maximum := maxFloat32(zero, one)
			expZero := math.Exp(float64(zero - maximum))
			expOne := math.Exp(float64(one - maximum))
			confidence = float32(expOne / (expZero + expOne))
		}
		rotate[index] = one > zero && confidence >= threshold
	}
	return rotate, nil
}

func maxFloat32(a, b float32) float32 {
	if a > b {
		return a
	}
	return b
}
