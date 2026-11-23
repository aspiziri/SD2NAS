// Package ingest imports a mounted camera card into Staging: scan, classify against the catalog, copy what is new, and record it.
package ingest

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aspiziri/SD2NAS/internal/media"
)

// StagingDir is the folder under the photos root that imports land in.
const StagingDir = "Staging"

// CardFile is a regular file found on a card.
type CardFile struct {
	Path string // OS path
	Rel  string // slash-separated, relative to the card root
	Size int64
	Kind media.Kind // "" for unsupported files
}

// ScanError is an entry below the card root that could not be read.
type ScanError struct {
	Rel string // the entry that couldn't be read
	Err error
}

// ScanResult is what Scan found on a card.
type ScanResult struct {
	Media       []CardFile  // files the rules import, sorted by Rel
	Unsupported []CardFile  // files neither imported nor ignored (Decision 10), sorted by Rel
	Errors      []ScanError // unreadable entries below the root
}

// unsupportedExts returns the distinct extensions of files, lowercase with
// the dot and sorted, with "" (no extension) last; nil for no files.
func unsupportedExts(files []CardFile) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		ext := strings.ToLower(path.Ext(f.Rel))
		if !seen[ext] {
			seen[ext] = true
			out = append(out, ext)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i] == "") != (out[j] == "") {
			return out[j] == ""
		}
		return out[i] < out[j]
	})
	return out
}

// ignoredExts are camera sidecars and metadata that hold no photo or video
// (Sony XML/BIN/IND/BNP/INT/INP/DAT/SMI, AVCHD BDM/MPL/CPI, Canon CTG,
// GoPro THM/LRV, DJI LRF, text files). Every other regular file the rules
// don't import is unsupported and blocks "safe to format" (Decision 10), so
// an unknown format is never lost by formatting. Lowercase, no dot.
var ignoredExts = map[string]bool{
	"xml": true, "bin": true, "ctg": true, "thm": true, "lrv": true, "lrf": true,
	"dat": true, "ind": true, "bdm": true, "mpl": true, "cpi": true, "smi": true,
	"bnp": true, "int": true, "inp": true, "txt": true,
}

// junkDirNames are directories (matched case-insensitively) that never hold
// camera media (Decision 11).
var junkDirNames = []string{"MISC", "System Volume Information", "$RECYCLE.BIN", "THMBNL"}

func skipDir(name, parent string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	for _, j := range junkDirNames {
		if strings.EqualFold(name, j) {
			return true
		}
	}
	return strings.EqualFold(name, "SUB") && strings.EqualFold(parent, "M4ROOT")
}

// Scan lists the importable and unsupported files below root, skipping
// camera and OS housekeeping folders, dot files, ignored sidecars and anything
// that is not a regular file. It only reads; it returns an error only when root itself is
// unusable.
func Scan(root string, rules media.Rules) (ScanResult, error) {
	info, err := os.Stat(root)
	if err != nil {
		return ScanResult{}, err
	}
	if !info.IsDir() {
		return ScanResult{}, fmt.Errorf("scan %s: not a directory", root)
	}
	var res ScanResult
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if path == root {
			return err // nil, or the root itself is unreadable
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if err != nil {
			res.Errors = append(res.Errors, ScanError{Rel: rel, Err: err})
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if skipDir(name, filepath.Base(filepath.Dir(path))) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") || !d.Type().IsRegular() {
			return nil
		}
		kind, ok := rules.KindOf(name)
		unsupported := false
		if !ok {
			if ignoredExts[strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))] {
				return nil
			}
			unsupported = true
		}
		fi, err := d.Info()
		if err != nil {
			res.Errors = append(res.Errors, ScanError{Rel: rel, Err: err})
			return nil
		}
		f := CardFile{Path: path, Rel: rel, Size: fi.Size(), Kind: kind}
		if unsupported {
			res.Unsupported = append(res.Unsupported, f)
		} else {
			res.Media = append(res.Media, f)
		}
		return nil
	})
	if walkErr != nil {
		return ScanResult{}, walkErr
	}
	byRel := func(s []CardFile) {
		sort.Slice(s, func(i, j int) bool { return s[i].Rel < s[j].Rel })
	}
	byRel(res.Media)
	byRel(res.Unsupported)
	return res, nil
}
