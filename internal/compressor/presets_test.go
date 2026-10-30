package compressor

import (
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
)

func TestScanPresetRequiresOriginalPDFs(t *testing.T) {
	for _, value := range []string{string(Scan300Color), string(Scan300Grayscale)} {
		if _, err := SelectPreset(value, false); err == nil {
			t.Fatalf("scan preset accepted a non-PDF input: %s", value)
		}
	}
	for _, value := range []string{"", string(DefaultPreset)} {
		if _, err := SelectPreset(value, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := SelectPreset("-dColorImageResolution=1", true); err == nil {
		t.Fatal("accepted user-supplied Ghostscript arguments")
	}
}

func TestPresetsThroughGhostscript(t *testing.T) {
	if _, err := exec.LookPath("gs"); err != nil {
		t.Skip("Ghostscript is required for the native PDF integration test")
	}
	t.Setenv("GS_SETTINGS", "/default")
	t.Setenv("GS_SETTINGS_FILE", "")
	for _, test := range []struct {
		name    string
		fixture string
		preset  Preset
		width   int
		height  int
		gray    bool
	}{
		{"default retains 600 DPI", "scan-600.pdf", DefaultPreset, 600, 400, false},
		{"color reduces 600 to 300 DPI", "scan-600.pdf", Scan300Color, 300, 200, false},
		{"grayscale reduces 600 to 300 DPI", "scan-600.pdf", Scan300Grayscale, 300, 200, true},
		{"color does not upscale 200 DPI", "scan-200.pdf", Scan300Color, 600, 400, false},
		{"grayscale does not upscale 200 DPI", "scan-200.pdf", Scan300Grayscale, 600, 400, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", test.fixture))
			if err != nil {
				t.Fatal(err)
			}
			input := filepath.Join(t.TempDir(), "scan.pdf")
			if err := os.WriteFile(input, data, 0600); err != nil {
				t.Fatal(err)
			}
			output, err := CompressPDFWithProgress(input, test.preset, nil)
			if err != nil {
				t.Fatal(err)
			}
			dims, err := api.PageDimsFile(output)
			if err != nil || len(dims) != 1 || dims[0].Width != 612 || dims[0].Height != 792 {
				t.Fatalf("page geometry changed: %v, %v", dims, err)
			}
			file, err := os.Open(output)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			pages, err := api.Images(file, nil, nil)
			if err != nil || len(pages) != 1 || len(pages[0]) != 1 {
				t.Fatalf("missing scanned image: %v, %v", pages, err)
			}
			for _, pic := range pages[0] {
				if pic.Width != test.width || pic.Height != test.height {
					t.Fatalf("image dimensions: %dx%d, want %dx%d", pic.Width, pic.Height, test.width, test.height)
				}
			}
			imageDir := t.TempDir()
			if err := api.ExtractImagesFile(output, imageDir, nil, nil); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(imageDir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("missing extracted image: %v, %v", entries, err)
			}
			for _, entry := range entries {
				imageFile, err := os.Open(filepath.Join(imageDir, entry.Name()))
				if err != nil {
					t.Fatal(err)
				}
				defer imageFile.Close()
				decoded, _, err := image.Decode(imageFile)
				if err != nil {
					t.Fatal(err)
				}
				r, g, b, _ := decoded.At(test.width/4, test.height/2).RGBA()
				if gray := r == g && g == b; gray != test.gray {
					t.Fatalf("grayscale=%v, want %v", gray, test.gray)
				}
			}
		})
	}
}
