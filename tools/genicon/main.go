// genicon 生成 cmd/gui/app.ico —— 与 makeAppIcon 同一图案（绿圆 + 白十字），
// 输出多分辨率 .ico（PNG 压缩项），供 rsrc -ico 嵌进 exe。
//
// 用法：go run ./tools/genicon -o cmd/gui/app.ico
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"image"
	"image/color"
	"image/png"
	"os"
)

var sizes = []int{16, 24, 32, 48, 64, 128, 256}

// render 在 size×size 上画出与 makeAppIcon 等比的绿圆 + 白十字。
func render(size int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	fg := color.RGBA{0x1D, 0x9E, 0x75, 0xFF}
	white := color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}

	c := float64(size) / 2
	r := c - float64(size)/16 // 半径：32px 时为 14（与原版 s/2-2 一致）

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := float64(x)+0.5-c, float64(y)+0.5-c
			if dx*dx+dy*dy <= r*r {
				img.Set(x, y, fg)
			}
		}
	}
	// 白十字：横条 32px 时为 x∈[8,24) y∈[14,18)；竖条 x∈[14,18) y∈[10,22)。
	k := float64(size) / 32.0
	fill := func(x0, y0, x1, y1 float64) {
		for y := int(y0 * k); y < int(y1*k); y++ {
			for x := int(x0 * k); x < int(x1*k); x++ {
				if x >= 0 && x < size && y >= 0 && y < size {
					img.Set(x, y, white)
				}
			}
		}
	}
	fill(8, 14, 24, 18)
	fill(14, 10, 18, 22)
	return img
}

func main() {
	out := flag.String("o", "cmd/gui/app.ico", "输出 .ico 路径")
	flag.Parse()

	type entry struct {
		w, h int
		data []byte
	}
	var entries []entry
	for _, s := range sizes {
		img := render(s)
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			panic(err)
		}
		entries = append(entries, entry{w: s, h: s, data: buf.Bytes()})
	}

	var buf bytes.Buffer
	// ICONDIR
	binary.Write(&buf, binary.LittleEndian, uint16(0))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint16(len(entries)))

	offset := 6 + 16*len(entries)
	for _, e := range entries {
		w, h := byte(e.w), byte(e.h)
		if e.w >= 256 {
			w = 0
		}
		if e.h >= 256 {
			h = 0
		}
		buf.WriteByte(w)                                             // width
		buf.WriteByte(h)                                             // height
		buf.WriteByte(0)                                             // color count
		buf.WriteByte(0)                                             // reserved
		binary.Write(&buf, binary.LittleEndian, uint16(1))           // planes
		binary.Write(&buf, binary.LittleEndian, uint16(32))          // bitcount
		binary.Write(&buf, binary.LittleEndian, uint32(len(e.data))) // size
		binary.Write(&buf, binary.LittleEndian, uint32(offset))      // offset
		offset += len(e.data)
	}
	for _, e := range entries {
		buf.Write(e.data)
	}

	if err := os.WriteFile(*out, buf.Bytes(), 0o644); err != nil {
		panic(err)
	}
}
