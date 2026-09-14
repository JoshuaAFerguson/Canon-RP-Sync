package thumb_test

import (
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"github.com/JoshuaAFerguson/canon-rp-sync/internal/thumb"
)

func writeJPEG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, img, nil); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateScalesDownPreservingAspect(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "IMG_0001.JPG")
	writeJPEG(t, src, 1600, 900)

	dst := filepath.Join(dir, "thumbs", "out.jpg")
	if err := thumb.Generate(src, dst, 320); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	f, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := jpeg.DecodeConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != 320 {
		t.Errorf("width = %d, want 320", cfg.Width)
	}
	if cfg.Height != 180 {
		t.Errorf("height = %d, want 180 (16:9 preserved)", cfg.Height)
	}
}

func TestGenerateLeavesSmallImagesAlone(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "small.jpg")
	writeJPEG(t, src, 100, 50)

	dst := filepath.Join(dir, "small-thumb.jpg")
	if err := thumb.Generate(src, dst, 480); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, _ := jpeg.DecodeConfig(f)
	if cfg.Width != 100 || cfg.Height != 50 {
		t.Errorf("size = %dx%d, want the original 100x50", cfg.Width, cfg.Height)
	}
}

func TestGenerateRejectsRAW(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "IMG_0001.CR3")
	if err := os.WriteFile(src, []byte("not a jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := thumb.Generate(src, filepath.Join(dir, "out.jpg"), 320)
	if !errors.Is(err, thumb.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if thumb.Supported(".cr3") {
		t.Error("CR3 should not be reported as supported")
	}
	if !thumb.Supported("JPG") {
		t.Error("JPG should be supported, with or without a dot")
	}
}

func TestCacheReusesGeneratedThumbnail(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "IMG_0002.JPG")
	writeJPEG(t, src, 800, 600)
	cacheDir := filepath.Join(dir, "cache")

	first, err := thumb.Cache(cacheDir, "abc123", src, 200)
	if err != nil {
		t.Fatalf("Cache: %v", err)
	}
	info, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}

	second, err := thumb.Cache(cacheDir, "abc123", src, 200)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Errorf("cache returned a different path: %q vs %q", second, first)
	}
	info2, err := os.Stat(second)
	if err != nil {
		t.Fatal(err)
	}
	if !info2.ModTime().Equal(info.ModTime()) {
		t.Error("second call regenerated the thumbnail instead of reusing it")
	}
}

func TestCacheMissingSource(t *testing.T) {
	if _, err := thumb.Cache(t.TempDir(), "id", filepath.Join(t.TempDir(), "gone.jpg"), 200); err == nil {
		t.Fatal("want an error for a missing source file")
	}
}
