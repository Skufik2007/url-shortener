// Package qrcode формирует PNG-изображение QR-кода для короткой ссылки.
//
// Кодирование (выбор версии, коды Рида–Соломона, размещение модулей) выполняет
// rsc.io/qr, а отрисовку пакет делает сам через стандартный image/png: встроенный
// в rsc.io/qr PNG-кодировщик падает при масштабе 1, а метод Image() не учитывает
// масштаб и белую рамку.
package qrcode

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"sync"

	"rsc.io/qr"
)

const (
	// MinSize и MaxSize — допустимые размеры стороны картинки в пикселях.
	MinSize = 64
	MaxSize = 1024
	// DefaultSize — размер по умолчанию.
	DefaultSize = 256

	// quietZone — обязательная белая рамка вокруг QR-кода в модулях (с каждой стороны).
	quietZone = 4
)

// Двухцветная палитра: PNG получается с глубиной 1 бит на пиксель.
var palette = color.Palette{color.White, color.Black}

// bufferPool — буферы кодировщика PNG (около 1 МБ на состояние сжатия) переиспользуются
// между запросами, а не выделяются заново для каждой картинки.
type bufferPool struct{ p sync.Pool }

func (b *bufferPool) Get() *png.EncoderBuffer {
	buf, _ := b.p.Get().(*png.EncoderBuffer)
	return buf
}

func (b *bufferPool) Put(buf *png.EncoderBuffer) { b.p.Put(buf) }

var encoder = png.Encoder{CompressionLevel: png.BestCompression, BufferPool: &bufferPool{}}

// PNG кодирует text в QR-код с уровнем коррекции ошибок M (восстанавливается
// около 15 % повреждённых модулей) и возвращает PNG со стороной не больше size
// пикселей. Размер приводится к диапазону [MinSize, MaxSize]. Модуль — квадрат
// из целого числа пикселей, поэтому сторона может быть немного меньше запрошенной.
func PNG(text string, size int) ([]byte, error) {
	size = min(max(size, MinSize), MaxSize)
	c, err := qr.Encode(text, qr.M)
	if err != nil {
		return nil, fmt.Errorf("qrcode: %w", err)
	}

	modules := c.Size + 2*quietZone
	scale := max(1, size/modules)
	side := modules * scale

	// Paletted по умолчанию заполнен индексом 0 — белым цветом.
	img := image.NewPaletted(image.Rect(0, 0, side, side), palette)
	for my := 0; my < c.Size; my++ {
		for mx := 0; mx < c.Size; mx++ {
			if !c.Black(mx, my) {
				continue
			}
			x0, y0 := (mx+quietZone)*scale, (my+quietZone)*scale
			for y := y0; y < y0+scale; y++ {
				row := img.Pix[y*img.Stride:]
				for x := x0; x < x0+scale; x++ {
					row[x] = 1 // чёрный
				}
			}
		}
	}

	var buf bytes.Buffer
	if err := encoder.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("qrcode: %w", err)
	}
	return buf.Bytes(), nil
}
