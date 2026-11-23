// Package ingest imports a mounted camera card into Staging: scan, classify against the catalog, copy what is new, and record it.
package ingest

import (
	"fmt"
	"io/fs"
	"os"
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
	Unsupported []CardFile  // media formats the rules don't import (Decision 10), sorted by Rel
	Errors      []ScanError // unreadable entries below the root
}

// unsupportedExts are media formats the rules do not import but that the
// user should be told about (Decision 10). Lowercase, no dot.
var unsupportedExts = map[string]bool{
	"heic": true, "heif": true, "hif": true, "tiff": true, "raf": true,
	"orf": true, "rw2": true, "pef": true, "srw": true, "x3f": true,
	"3fr": true, "iiq": true, "gpr": true, "crw": true, "nrw": true,
	"srf": true, "sr2": true, "m4v": true, "3gp": true, "mts": true,
	"m2ts": true, "mxf": true, "braw": true, "r3d": true, "insv": true,
	"insp": true,
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

// Scan lists the importable and unsupported media files below root, skipping
// camera and OS housekeeping folders, dot files and anything that is not a
// regular file. It only reads; it returns an error only when root itself is
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
			ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
			if !unsupportedExts[ext] {
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
