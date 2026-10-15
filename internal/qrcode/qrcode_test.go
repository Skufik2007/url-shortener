package qrcode

import (
	"bytes"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/makiuchi-d/gozxing"
	gozxingqr "github.com/makiuchi-d/gozxing/qrcode"
)

// decode расшифровывает QR-код с картинки независимым декодером (порт ZXing).
func decode(t *testing.T, img image.Image) string {
	t.Helper()
	bmp, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		t.Fatal(err)
	}
	res, err := gozxingqr.NewQRCodeReader().Decode(bmp, nil)
	if err != nil {
		t.Fatalf("QR-код не распознан: %v", err)
	}
	return res.GetText()
}

func TestPNGRoundTrip(t *testing.T) {
	tests := []string{
		"http://localhost/sale2026",
		"https://sokr.ru/aB3dE7x",
		"http://192.168.1.5/" + strings.Repeat("x", 32), // самый длинный алиас
	}
	for _, text := range tests {
		t.Run(text, func(t *testing.T) {
			data, err := PNG(text, DefaultSize)
			if err != nil {
				t.Fatal(err)
			}
			img, err := png.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("некорректный PNG: %v", err)
			}
			if got := decode(t, img); got != text {
				t.Errorf("расшифровано %q, ожидалось %q", got, text)
			}
		})
	}
}

func TestPNGSize(t *testing.T) {
	tests := []struct {
		name     string
		size     int
		wantMin  int // сторона не меньше (масштаб целый, поэтому возможен недобор)
		wantMax  int
		squareOK bool
	}{
		{"по умолчанию", DefaultSize, DefaultSize * 3 / 4, DefaultSize, true},
		{"максимум", MaxSize, MaxSize * 3 / 4, MaxSize, true},
		{"меньше минимума приводится к минимуму", 10, MinSize / 2, MinSize, true},
		{"больше максимума приводится к максимуму", 5000, MaxSize * 3 / 4, MaxSize, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, err := PNG("http://localhost/abc1234", tc.size)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := png.DecodeConfig(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Width != cfg.Height {
				t.Errorf("картинка не квадратная: %dx%d", cfg.Width, cfg.Height)
			}
			if cfg.Width < tc.wantMin || cfg.Width > tc.wantMax {
				t.Errorf("сторона %d, ожидалось от %d до %d", cfg.Width, tc.wantMin, tc.wantMax)
			}
		})
	}
}

func TestPNGTooLong(t *testing.T) {
	// Версия 40 с уровнем M вмещает 2331 байт; более длинный текст закодировать нельзя.
	if _, err := PNG(strings.Repeat("a", 3000), DefaultSize); err == nil {
		t.Fatal("ожидалась ошибка для слишком длинного текста")
	}
}

func BenchmarkPNG(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := PNG("http://localhost/aB3dE7x", DefaultSize); err != nil {
			b.Fatal(err)
		}
	}
}
