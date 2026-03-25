package web

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/draw"
	"image/png"
)

var (
	favicon16  = mustPNG(16)
	favicon32  = mustPNG(32)
	faviconICO = mustICO(favicon16)
)

func Favicon16() []byte {
	return cloneBytes(favicon16)
}

func Favicon32() []byte {
	return cloneBytes(favicon32)
}

func FaviconICO() []byte {
	return cloneBytes(faviconICO)
}

func mustPNG(size int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.NRGBA{R: 15, G: 23, B: 42, A: 255}}, image.Point{}, draw.Src)

	accent := color.NRGBA{R: 14, G: 165, B: 233, A: 255}
	light := color.NRGBA{R: 248, G: 250, B: 252, A: 255}
	border := size / 8
	for y := border; y < size-border; y++ {
		for x := border; x < size-border; x++ {
			img.Set(x, y, accent)
		}
	}
	center := size / 2
	for y := border + 1; y < size-border-1; y++ {
		img.Set(center, y, light)
	}
	for x := border + 1; x < size-border-1; x++ {
		img.Set(x, center, light)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func mustICO(pngBytes []byte) []byte {
	var buf bytes.Buffer

	type iconDir struct {
		Reserved uint16
		Type     uint16
		Count    uint16
	}
	type iconEntry struct {
		Width       uint8
		Height      uint8
		ColorCount  uint8
		Reserved    uint8
		Planes      uint16
		BitCount    uint16
		BytesInRes  uint32
		ImageOffset uint32
	}

	if err := binary.Write(&buf, binary.LittleEndian, iconDir{Type: 1, Count: 1}); err != nil {
		panic(err)
	}
	if err := binary.Write(&buf, binary.LittleEndian, iconEntry{
		Width:       16,
		Height:      16,
		Planes:      1,
		BitCount:    32,
		BytesInRes:  uint32(len(pngBytes)),
		ImageOffset: 6 + 16,
	}); err != nil {
		panic(err)
	}
	if _, err := buf.Write(pngBytes); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func cloneBytes(source []byte) []byte {
	cloned := make([]byte, len(source))
	copy(cloned, source)
	return cloned
}
