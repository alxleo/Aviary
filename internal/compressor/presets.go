package compressor

import (
	"fmt"

	"github.com/rmitchellscott/aviary/internal/config"
)

type Preset string

const (
	DefaultPreset    Preset = "default"
	Scan300Color     Preset = "scan_300_color"
	Scan300Grayscale Preset = "scan_300_grayscale"
)

// SelectPreset limits scan processing to requests containing only original PDFs.
// Converted Word, image and article inputs must never inherit scan resampling.
func SelectPreset(value string, pdfOnly bool) (Preset, error) {
	preset := Preset(value)
	if preset == "" || preset == DefaultPreset {
		return DefaultPreset, nil
	}
	switch preset {
	case Scan300Color, Scan300Grayscale:
		if !pdfOnly {
			return "", fmt.Errorf("300 DPI scan presets require PDF inputs")
		}
		return preset, nil
	default:
		return "", fmt.Errorf("unknown compression preset %q", value)
	}
}

func (preset Preset) arguments() ([]string, error) {
	if preset == "" || preset == DefaultPreset {
		return []string{"-dPDFSETTINGS=" + config.Get("GS_SETTINGS", "/ebook")}, nil
	}
	if _, err := SelectPreset(string(preset), true); err != nil {
		return nil, err
	}
	args := []string{
		"-dPDFSETTINGS=/default",
		"-dAutoRotatePages=/None",
		"-dDownsampleColorImages=true", "-dColorImageResolution=300",
		"-dColorImageDownsampleThreshold=1.0", "-dColorImageDownsampleType=/Bicubic",
		"-dDownsampleGrayImages=true", "-dGrayImageResolution=300",
		"-dGrayImageDownsampleThreshold=1.0", "-dGrayImageDownsampleType=/Bicubic",
		"-dDownsampleMonoImages=true", "-dMonoImageResolution=300",
		"-dMonoImageDownsampleThreshold=1.0", "-dMonoImageDownsampleType=/Subsample",
	}
	if preset == Scan300Grayscale {
		args = append(args, "-sColorConversionStrategy=Gray")
	}
	return args, nil
}
