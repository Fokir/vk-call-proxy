package captcha

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"math"
)

// sliderContent holds the parsed response from captchaNotRobot.getContent.
type sliderContent struct {
	Image     string `json:"image"`     // base64-encoded image
	Extension string `json:"extension"` // "jpeg" or "png"
	Steps     []int  `json:"steps"`     // [gridSize, swapPair0a, swapPair0b, ...]
	Status    string `json:"status"`
}

// sliderPuzzle holds the parsed puzzle data ready for solving.
type sliderPuzzle struct {
	img       image.Image
	gridSize  int   // e.g. 5 for 5x5
	swapPairs []int // even-length array of swap pair indices
	attempts  int   // remaining attempts (0 if unknown)
}

// parseSliderContent parses the getContent API response into a solvable puzzle.
func parseSliderContent(respBody []byte) (*sliderPuzzle, error) {
	var outer struct {
		Response sliderContent `json:"response"`
	}
	if err := json.Unmarshal(respBody, &outer); err != nil {
		return nil, fmt.Errorf("parse getContent response: %w", err)
	}
	sc := outer.Response
	if sc.Status != "OK" {
		return nil, fmt.Errorf("getContent status: %s", sc.Status)
	}
	if len(sc.Steps) < 3 {
		return nil, fmt.Errorf("steps too short: %d", len(sc.Steps))
	}

	// Decode image.
	imgData, err := base64.StdEncoding.DecodeString(sc.Image)
	if err != nil {
		return nil, fmt.Errorf("decode image base64: %w", err)
	}
	img, _, err := image.Decode(bytes.NewReader(imgData))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}

	// Parse steps: first element = grid size, rest = swap pairs.
	// If remaining count is odd, last element = attempts count.
	gridSize := sc.Steps[0]
	rest := make([]int, len(sc.Steps)-1)
	copy(rest, sc.Steps[1:])

	var attempts int
	if len(rest)%2 != 0 {
		attempts = rest[len(rest)-1]
		rest = rest[:len(rest)-1]
	}

	return &sliderPuzzle{
		img:       img,
		gridSize:  gridSize,
		swapPairs: rest,
		attempts:  attempts,
	}, nil
}

// tileEdges holds pre-extracted edge pixel data for a single tile.
type tileEdges struct {
	right  [][3]float64 // tileH * 2 pixels (2 columns × tileH rows)
	left   [][3]float64
	bottom [][3]float64 // tileW * 2 pixels (tileW columns × 2 rows)
	top    [][3]float64
}

// solveSlider finds the optimal slider position using edge-matching analysis.
// Returns the answer array (activeSteps slice) to be base64-encoded for the API.
func solveSlider(p *sliderPuzzle) ([]int, error) {
	gridSize := p.gridSize
	numTiles := gridSize * gridSize
	maxPos := len(p.swapPairs) / 2

	if maxPos == 0 {
		return nil, fmt.Errorf("no swap pairs to solve")
	}

	bounds := p.img.Bounds()
	imgW := bounds.Dx()
	imgH := bounds.Dy()
	tileW := imgW / gridSize
	tileH := imgH / gridSize

	if tileW < 4 || tileH < 4 {
		return nil, fmt.Errorf("tiles too small: %dx%d", tileW, tileH)
	}

	// Pre-extract edge pixel data for each tile to avoid repeated image access.

	edges := make([]tileEdges, numTiles)
	for idx := 0; idx < numTiles; idx++ {
		sr := idx / gridSize
		sc := idx % gridSize
		ox := bounds.Min.X + sc*tileW
		oy := bounds.Min.Y + sr*tileH

		te := tileEdges{
			right:  make([][3]float64, tileH*2),
			left:   make([][3]float64, tileH*2),
			bottom: make([][3]float64, tileW*2),
			top:    make([][3]float64, tileW*2),
		}

		for y := 0; y < tileH; y++ {
			// Right edge: last 2 columns.
			for d := 0; d < 2; d++ {
				r, g, b := rgbAt(p.img, ox+tileW-2+d, oy+y)
				te.right[y*2+d] = [3]float64{r, g, b}
			}
			// Left edge: first 2 columns.
			for d := 0; d < 2; d++ {
				r, g, b := rgbAt(p.img, ox+d, oy+y)
				te.left[y*2+d] = [3]float64{r, g, b}
			}
		}
		for x := 0; x < tileW; x++ {
			// Bottom edge: last 2 rows.
			for d := 0; d < 2; d++ {
				r, g, b := rgbAt(p.img, ox+x, oy+tileH-2+d)
				te.bottom[x*2+d] = [3]float64{r, g, b}
			}
			// Top edge: first 2 rows.
			for d := 0; d < 2; d++ {
				r, g, b := rgbAt(p.img, ox+x, oy+d)
				te.top[x*2+d] = [3]float64{r, g, b}
			}
		}
		edges[idx] = te
	}

	// For each slider position, compute edge score.
	perm := make([]int, numTiles)
	bestPos := 0
	bestScore := math.MaxFloat64

	for w := 0; w <= maxPos; w++ {
		// Build permutation incrementally.
		if w == 0 {
			for i := range perm {
				perm[i] = i
			}
		} else {
			a := p.swapPairs[(w-1)*2]
			b := p.swapPairs[(w-1)*2+1]
			perm[a], perm[b] = perm[b], perm[a]
		}

		score := edgeScore(perm, edges, gridSize)
		if score < bestScore {
			bestScore = score
			bestPos = w
		}
	}

	// The answer is swapPairs[0 : 2*bestPos].
	answerLen := 2 * bestPos
	answer := make([]int, answerLen)
	copy(answer, p.swapPairs[:answerLen])
	return answer, nil
}

// edgeScore computes the total pixel difference at tile boundaries for a given permutation.
// Lower score = better alignment.
func edgeScore(perm []int, edges []tileEdges, gridSize int) float64 {
	var score float64
	for r := 0; r < gridSize; r++ {
		for c := 0; c < gridSize; c++ {
			src := perm[r*gridSize+c]

			// Horizontal: right edge of (r,c) vs left edge of (r,c+1).
			if c+1 < gridSize {
				rightSrc := perm[r*gridSize+c+1]
				score += edgeDiff(edges[src].right, edges[rightSrc].left)
			}

			// Vertical: bottom edge of (r,c) vs top edge of (r+1,c).
			if r+1 < gridSize {
				belowSrc := perm[(r+1)*gridSize+c]
				score += edgeDiff(edges[src].bottom, edges[belowSrc].top)
			}
		}
	}
	return score
}

// edgeDiff computes the mean absolute difference between two edge pixel arrays.
func edgeDiff(a, b [][3]float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 255 * 3
	}
	var sum float64
	for i := range a {
		sum += math.Abs(a[i][0] - b[i][0])
		sum += math.Abs(a[i][1] - b[i][1])
		sum += math.Abs(a[i][2] - b[i][2])
	}
	return sum / float64(len(a))
}

// encodeSliderAnswer encodes the answer array as base64 JSON for the API.
func encodeSliderAnswer(answer []int) string {
	val := struct {
		Value []int `json:"value"`
	}{Value: answer}
	j, _ := json.Marshal(val)
	return base64.StdEncoding.EncodeToString(j)
}

func rgbAt(img image.Image, x, y int) (float64, float64, float64) {
	r, g, b, _ := img.At(x, y).RGBA()
	return float64(r >> 8), float64(g >> 8), float64(b >> 8)
}
