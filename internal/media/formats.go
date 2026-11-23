// Package media classifies, dates, names, hashes and copies camera files. It is pure functions: no service state.
package media

import (
	"path/filepath"
	"strings"
)

// Kind is the class of a camera file.
type Kind string

const (
	KindImage Kind = "image"
	KindRaw   Kind = "raw"
	KindVideo Kind = "video"
)

// Rules is the import rule set: which extensions are imported as which
// kind, and which subfolder of a session folder each kind goes in
// ("" = the folder itself).
type Rules struct {
	ImageFormats []string // lowercase, no dot
	RawFormats   []string
	VideoFormats []string
	ImageSubdir  string
	RawSubdir    string
	VideoSubdir  string
}

// DefaultRules returns CameraImport's format lists and subfolders, plus cr3.
func DefaultRules() Rules {
	return Rules{
		ImageFormats: []string{"jpg", "jpeg", "png", "gif", "tif"},
		RawFormats:   []string{"arw", "cr2", "cr3", "nef", "dng"},
		VideoFormats: []string{"mp4", "avi", "mov"},
		ImageSubdir:  "",
		RawSubdir:    "Capture",
		VideoSubdir:  "Videos",
	}
}

// KindOf classifies name by its extension, case-insensitively. It reports
// false for files the rules do not import.
func (r Rules) KindOf(name string) (Kind, bool) {
	ext := strings.TrimPrefix(filepath.Ext(name), ".")
	if ext == "" {
		return "", false
	}
	if hasFormat(r.ImageFormats, ext) {
		return KindImage, true
	}
	if hasFormat(r.VideoFormats, ext) {
		return KindVideo, true
	}
	if hasFormat(r.RawFormats, ext) {
		return KindRaw, true
	}
	return "", false
}

func hasFormat(list []string, ext string) bool {
	for _, f := range list {
		if strings.EqualFold(f, ext) {
			return true
		}
	}
	return false
}

// TypeSubdir returns the session subfolder for kind, "" for the folder
// itself or for an unknown kind.
func TypeSubdir(kind Kind, r Rules) string {
	switch kind {
	case KindImage:
		return r.ImageSubdir
	case KindRaw:
		return r.RawSubdir
	case KindVideo:
		return r.VideoSubdir
	}
	return ""
}
