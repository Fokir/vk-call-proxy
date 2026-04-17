package luamod

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

const imgTypeName = "image"

// RegisterImg registers the `img` global table into L.
func RegisterImg(L *lua.LState) {
	tbl := L.NewTable()
	L.SetField(tbl, "load", L.NewFunction(imgLoad))
	L.SetField(tbl, "decode_base64", L.NewFunction(imgDecodeBase64))
	L.SetField(tbl, "width", L.NewFunction(imgWidth))
	L.SetField(tbl, "height", L.NewFunction(imgHeight))
	L.SetField(tbl, "pixel", L.NewFunction(imgPixel))
	L.SetField(tbl, "crop", L.NewFunction(imgCrop))
	L.SetField(tbl, "resize", L.NewFunction(imgResize))
	L.SetField(tbl, "grayscale", L.NewFunction(imgGrayscale))
	L.SetField(tbl, "diff", L.NewFunction(imgDiff))
	L.SetField(tbl, "edge_detect", L.NewFunction(imgEdgeDetect))
	L.SetField(tbl, "encode", L.NewFunction(imgEncode))
	L.SetGlobal("img", tbl)
}

// newImageUserdata wraps an image.Image into a Lua userdata.
func newImageUserdata(L *lua.LState, img image.Image) *lua.LUserData {
	ud := L.NewUserData()
	ud.Value = img
	return ud
}

// checkImage extracts an image.Image from a Lua userdata argument at position n.
func checkImage(L *lua.LState, n int) image.Image {
	ud := L.CheckUserData(n)
	img, ok := ud.Value.(image.Image)
	if !ok {
		L.ArgError(n, "expected image userdata")
		return nil
	}
	return img
}

// decodeImageBytes decodes PNG or JPEG from raw bytes.
func decodeImageBytes(data []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	return img, err
}

// imgLoad implements img.load(bytes_string) → userdata.
func imgLoad(L *lua.LState) int {
	data := L.CheckString(1)
	img, err := decodeImageBytes([]byte(data))
	if err != nil {
		L.RaiseError("img.load: %v", err)
		return 0
	}
	L.Push(newImageUserdata(L, img))
	return 1
}

// imgDecodeBase64 implements img.decode_base64(str) → userdata.
func imgDecodeBase64(L *lua.LState) int {
	str := L.CheckString(1)

	// Strip optional data URI prefix: "data:image/...;base64,"
	if idx := strings.Index(str, ";base64,"); idx >= 0 {
		str = str[idx+len(";base64,"):]
	}

	data, err := base64.StdEncoding.DecodeString(str)
	if err != nil {
		// Try RawStdEncoding (no padding)
		data, err = base64.RawStdEncoding.DecodeString(str)
		if err != nil {
			L.RaiseError("img.decode_base64: base64 decode: %v", err)
			return 0
		}
	}

	img, err := decodeImageBytes(data)
	if err != nil {
		L.RaiseError("img.decode_base64: image decode: %v", err)
		return 0
	}
	L.Push(newImageUserdata(L, img))
	return 1
}

// imgWidth implements img.width(ud) → int.
func imgWidth(L *lua.LState) int {
	img := checkImage(L, 1)
	if img == nil {
		return 0
	}
	L.Push(lua.LNumber(img.Bounds().Dx()))
	return 1
}

// imgHeight implements img.height(ud) → int.
func imgHeight(L *lua.LState) int {
	img := checkImage(L, 1)
	if img == nil {
		return 0
	}
	L.Push(lua.LNumber(img.Bounds().Dy()))
	return 1
}

// imgPixel implements img.pixel(ud, x, y) → r, g, b, a (0-255, 0-based coords).
func imgPixel(L *lua.LState) int {
	src := checkImage(L, 1)
	if src == nil {
		return 0
	}
	x := L.CheckInt(2)
	y := L.CheckInt(3)
	b := src.Bounds()
	r32, g32, b32, a32 := src.At(b.Min.X+x, b.Min.Y+y).RGBA()
	// RGBA() returns values in [0, 65535]; convert to [0, 255]
	L.Push(lua.LNumber(r32 >> 8))
	L.Push(lua.LNumber(g32 >> 8))
	L.Push(lua.LNumber(b32 >> 8))
	L.Push(lua.LNumber(a32 >> 8))
	return 4
}

// imgCrop implements img.crop(ud, x, y, w, h) → new userdata.
func imgCrop(L *lua.LState) int {
	src := checkImage(L, 1)
	if src == nil {
		return 0
	}
	x := L.CheckInt(2)
	y := L.CheckInt(3)
	w := L.CheckInt(4)
	h := L.CheckInt(5)

	b := src.Bounds()
	rect := image.Rect(b.Min.X+x, b.Min.Y+y, b.Min.X+x+w, b.Min.Y+y+h)

	// Try SubImage first (most image types support it)
	type subImager interface {
		SubImage(r image.Rectangle) image.Image
	}
	if si, ok := src.(subImager); ok {
		L.Push(newImageUserdata(L, si.SubImage(rect)))
		return 1
	}

	// Fallback: manual copy into a new RGBA
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), src, rect.Min, draw.Src)
	L.Push(newImageUserdata(L, dst))
	return 1
}

// imgResize implements img.resize(ud, w, h) → new userdata (nearest-neighbor).
func imgResize(L *lua.LState) int {
	src := checkImage(L, 1)
	if src == nil {
		return 0
	}
	dstW := L.CheckInt(2)
	dstH := L.CheckInt(3)
	if dstW <= 0 || dstH <= 0 {
		L.RaiseError("img.resize: width and height must be positive")
		return 0
	}

	srcB := src.Bounds()
	srcW := srcB.Dx()
	srcH := srcB.Dy()

	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	for dy := 0; dy < dstH; dy++ {
		for dx := 0; dx < dstW; dx++ {
			sx := dx * srcW / dstW
			sy := dy * srcH / dstH
			dst.Set(dx, dy, src.At(srcB.Min.X+sx, srcB.Min.Y+sy))
		}
	}
	L.Push(newImageUserdata(L, dst))
	return 1
}

// imgGrayscale implements img.grayscale(ud) → new userdata (grayscale RGBA).
func imgGrayscale(L *lua.LState) int {
	src := checkImage(L, 1)
	if src == nil {
		return 0
	}
	b := src.Bounds()
	w := b.Dx()
	h := b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for dy := 0; dy < h; dy++ {
		for dx := 0; dx < w; dx++ {
			r32, g32, b32, a32 := src.At(b.Min.X+dx, b.Min.Y+dy).RGBA()
			// Luma: 0.299R + 0.587G + 0.114B (using integer coefficients scaled by 1000)
			luma := (299*r32 + 587*g32 + 114*b32) / 1000
			// luma is in [0, 65535*1000/1000] = [0, 65535]; shift to [0, 255]
			luma8 := uint8(luma >> 8)
			dst.SetRGBA(dx, dy, color.RGBA{luma8, luma8, luma8, uint8(a32 >> 8)})
		}
	}
	L.Push(newImageUserdata(L, dst))
	return 1
}

// imgDiff implements img.diff(ud1, ud2) → new userdata.
// Absolute pixel difference per channel. Images must be the same size.
func imgDiff(L *lua.LState) int {
	src1 := checkImage(L, 1)
	if src1 == nil {
		return 0
	}
	src2 := checkImage(L, 2)
	if src2 == nil {
		return 0
	}

	b1 := src1.Bounds()
	b2 := src2.Bounds()
	if b1.Dx() != b2.Dx() || b1.Dy() != b2.Dy() {
		L.RaiseError("img.diff: images must be the same size (%dx%d vs %dx%d)",
			b1.Dx(), b1.Dy(), b2.Dx(), b2.Dy())
		return 0
	}

	w := b1.Dx()
	h := b1.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for dy := 0; dy < h; dy++ {
		for dx := 0; dx < w; dx++ {
			r1, g1, bl1, a1 := src1.At(b1.Min.X+dx, b1.Min.Y+dy).RGBA()
			r2, g2, bl2, a2 := src2.At(b2.Min.X+dx, b2.Min.Y+dy).RGBA()
			dst.SetRGBA(dx, dy, color.RGBA{
				absDiff8(r1, r2),
				absDiff8(g1, g2),
				absDiff8(bl1, bl2),
				absDiff8(a1, a2),
			})
		}
	}
	L.Push(newImageUserdata(L, dst))
	return 1
}

// absDiff8 returns |a - b| >> 8, clamped to uint8.
func absDiff8(a, b uint32) uint8 {
	if a >= b {
		return uint8((a - b) >> 8)
	}
	return uint8((b - a) >> 8)
}

// imgEdgeDetect implements img.edge_detect(ud) → new userdata.
// Applies 3x3 Sobel operator on the grayscale version of the image.
func imgEdgeDetect(L *lua.LState) int {
	srcUD := checkImage(L, 1)
	if srcUD == nil {
		return 0
	}

	b := srcUD.Bounds()
	w := b.Dx()
	h := b.Dy()

	// Build a grayscale float buffer for Sobel convolution.
	gray := make([]float64, w*h)
	for dy := 0; dy < h; dy++ {
		for dx := 0; dx < w; dx++ {
			r32, g32, b32, _ := srcUD.At(b.Min.X+dx, b.Min.Y+dy).RGBA()
			luma := (299*r32 + 587*g32 + 114*b32) / 1000
			gray[dy*w+dx] = float64(luma >> 8)
		}
	}

	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for dy := 0; dy < h; dy++ {
		for dx := 0; dx < w; dx++ {
			gx := sobelGx(gray, w, h, dx, dy)
			gy := sobelGy(gray, w, h, dx, dy)
			mag := math.Sqrt(gx*gx + gy*gy)
			if mag > 255 {
				mag = 255
			}
			v := uint8(mag)
			dst.SetRGBA(dx, dy, color.RGBA{v, v, v, 255})
		}
	}
	L.Push(newImageUserdata(L, dst))
	return 1
}

// grayAt returns the grayscale value at (x, y), clamping coordinates to [0, w-1] x [0, h-1].
func grayAt(gray []float64, w, h, x, y int) float64 {
	if x < 0 {
		x = 0
	} else if x >= w {
		x = w - 1
	}
	if y < 0 {
		y = 0
	} else if y >= h {
		y = h - 1
	}
	return gray[y*w+x]
}

// sobelGx computes the Sobel Gx gradient at (x, y).
//
// Kernel:
//
//	-1  0  1
//	-2  0  2
//	-1  0  1
func sobelGx(gray []float64, w, h, x, y int) float64 {
	return -grayAt(gray, w, h, x-1, y-1) + grayAt(gray, w, h, x+1, y-1) +
		-2*grayAt(gray, w, h, x-1, y) + 2*grayAt(gray, w, h, x+1, y) +
		-grayAt(gray, w, h, x-1, y+1) + grayAt(gray, w, h, x+1, y+1)
}

// sobelGy computes the Sobel Gy gradient at (x, y).
//
// Kernel:
//
//	-1 -2 -1
//	 0  0  0
//	 1  2  1
func sobelGy(gray []float64, w, h, x, y int) float64 {
	return -grayAt(gray, w, h, x-1, y-1) - 2*grayAt(gray, w, h, x, y-1) - grayAt(gray, w, h, x+1, y-1) +
		grayAt(gray, w, h, x-1, y+1) + 2*grayAt(gray, w, h, x, y+1) + grayAt(gray, w, h, x+1, y+1)
}

// imgEncode implements img.encode(ud, format_string) → bytes string.
// Supported formats: "png", "jpeg".
func imgEncode(L *lua.LState) int {
	src := checkImage(L, 1)
	if src == nil {
		return 0
	}
	format := strings.ToLower(L.CheckString(2))

	var buf bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&buf, src)
	case "jpeg", "jpg":
		err = jpeg.Encode(&buf, src, nil)
	default:
		L.RaiseError("img.encode: unsupported format %q (use \"png\" or \"jpeg\")", format)
		return 0
	}
	if err != nil {
		L.RaiseError("img.encode: %v", err)
		return 0
	}
	L.Push(lua.LString(buf.Bytes()))
	return 1
}
