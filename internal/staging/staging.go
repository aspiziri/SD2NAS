// Package staging reads the folders under Staging/ for the web UI. M4 adds
// the operations that change them.
package staging

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
)

// Dir is the folder under the photos root that imports land in. It must
// equal ingest.StagingDir (dir_test.go checks).
const Dir = "Staging"

// MaxSamples is how many thumbnails a folder card shows.
const MaxSamples = 5

var ErrBadKind = errors.New("staging: kind must be jpg, raw or video")

type Service struct {
	db         *catalog.DB
	photosRoot string
}

func New(db *catalog.DB, photosRoot string) *Service { return &Service{db: db, photosRoot: photosRoot} }

type KindCounts struct {
	JPG   int `json:"jpg"`
	Raw   int `json:"raw"`
	Video int `json:"video"`
}

type Sample struct {
	SHA256 string `json:"sha256"`
	Kind   string `json:"kind"` // jpg | raw | video
}

// Folder is one staging folder as the UI shows it.
type Folder struct {
	ID            int64      `json:"id"`
	Name          string     `json:"name"`
	Date          string     `json:"date"`  // the name's leading YYYY-MM-DD, "" if none
	Title         string     `json:"title"` // the text after "YYYY-MM-DD - ", or the whole name if it has no date
	Dates         []string   `json:"dates"`
	CreatedAt     time.Time  `json:"created_at"`
	Files         int        `json:"files"`
	Bytes         int64      `json:"bytes"`
	Counts        KindCounts `json:"counts"`
	FirstCapture  string     `json:"first_capture,omitempty"` // wall clock, catalog.CaptureLayout
	LastCapture   string     `json:"last_capture,omitempty"`
	Samples       []Sample   `json:"samples"`
	ClockWarnings int        `json:"clock_warnings"` // files with at least one clock warning
	Warnings      []string   `json:"warnings"`
}

type Listing struct {
	Folders   []Folder `json:"folders"`   // ordered by name
	Untracked []string `json:"untracked"` // directories in Staging/ with no catalog row (M4 adopts them)
}

type File struct {
	SHA256        string   `json:"sha256"`
	Name          string   `json:"name"` // base name
	Path          string   `json:"path"` // slash path inside the folder, e.g. "Capture/x.ARW"
	Kind          string   `json:"kind"`
	Size          int64    `json:"size"`
	CaptureTime   string   `json:"capture_time,omitempty"`
	TZOffset      string   `json:"tz_offset,omitempty"`
	ClockWarnings []string `json:"clock_warnings,omitempty"`
}

type FolderFiles struct {
	Folder Folder `json:"folder"`
	Files  []File `json:"files"`
}

var nameRE = regexp.MustCompile(`^([0-9]{4}-[0-9]{2}-[0-9]{2})(?: - (.+))?$`)

// SplitName splits a folder name into its leading date and its title. Names
// are text: the date is never validated as a calendar date.
func SplitName(name string) (date, title string) {
	if m := nameRE.FindStringSubmatch(name); m != nil {
		return m[1], m[2]
	}
	return "", name
}

// KindName is the API name of a catalog media kind.
func KindName(k media.Kind) string {
	switch k {
	case media.KindImage:
		return "jpg"
	case media.KindRaw:
		return "raw"
	case media.KindVideo:
		return "video"
	}
	return string(k)
}

// List returns every staging folder with its summary, plus the directories
// on disk the catalog does not know.
func (s *Service) List(ctx context.Context) (Listing, error) {
	rows, err := s.db.StagingFolders(ctx)
	if err != nil {
		return Listing{}, err
	}
	assets, err := s.db.AssetsUnder(ctx, Dir+"/", catalog.StateStaged)
	if err != nil {
		return Listing{}, err
	}
	ws, err := s.db.ClockWarningsUnder(ctx, Dir+"/", catalog.StateStaged)
	if err != nil {
		return Listing{}, err
	}
	onDisk, err := s.diskFolders()
	if err != nil {
		return Listing{}, err
	}
	byFolder := map[string][]catalog.Asset{}
	for _, a := range assets {
		name, _, ok := strings.Cut(strings.TrimPrefix(a.RelPath, Dir+"/"), "/")
		if !ok {
			// Directly in Staging/: ingest never puts a file there, and no
			// folder owns it.
			continue
		}
		byFolder[name] = append(byFolder[name], a)
	}
	warned := warnedFiles(ws)
	out := Listing{Folders: []Folder{}, Untracked: []string{}}
	known := map[string]bool{}
	for _, row := range rows {
		known[row.Name] = true
		out.Folders = append(out.Folders, summarize(row, sortFiles(byFolder[row.Name]), warned, onDisk[row.Name]))
	}
	for name := range onDisk {
		if !known[name] {
			out.Untracked = append(out.Untracked, name)
		}
	}
	slices.Sort(out.Untracked)
	return out, nil
}

// diskFolders returns the visible directories in Staging/. A missing
// Staging/ means no import has happened yet.
func (s *Service) diskFolders() (map[string]bool, error) {
	entries, err := os.ReadDir(filepath.Join(s.photosRoot, Dir))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s/: %w", Dir, err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out[e.Name()] = true
		}
	}
	return out, nil
}

// sortFiles returns a copy of assets sorted by base name, then by path.
func sortFiles(assets []catalog.Asset) []catalog.Asset {
	out := slices.Clone(assets)
	slices.SortStableFunc(out, func(a, b catalog.Asset) int {
		if c := strings.Compare(path.Base(a.RelPath), path.Base(b.RelPath)); c != 0 {
			return c
		}
		return strings.Compare(a.RelPath, b.RelPath)
	})
	return out
}

func summarize(row catalog.StagingFolder, sorted []catalog.Asset, warned map[string][]string, onDisk bool) Folder {
	date, title := SplitName(row.Name)
	f := Folder{
		ID: row.ID, Name: row.Name, Date: date, Title: title,
		Dates: row.Dates, CreatedAt: row.CreatedAt, Files: len(sorted),
		Samples: pickSamples(sorted, MaxSamples), Warnings: []string{},
	}
	if f.Dates == nil {
		f.Dates = []string{}
	}
	var first, last time.Time
	for _, a := range sorted {
		f.Bytes += a.Size
		switch a.Kind {
		case media.KindImage:
			f.Counts.JPG++
		case media.KindRaw:
			f.Counts.Raw++
		case media.KindVideo:
			f.Counts.Video++
		}
		if !a.CaptureTime.IsZero() {
			if first.IsZero() || a.CaptureTime.Before(first) {
				first = a.CaptureTime
			}
			if last.IsZero() || a.CaptureTime.After(last) {
				last = a.CaptureTime
			}
		}
		if len(warned[a.SHA256]) > 0 {
			f.ClockWarnings++
		}
	}
	if !first.IsZero() {
		f.FirstCapture = first.Format(catalog.CaptureLayout)
		f.LastCapture = last.Format(catalog.CaptureLayout)
	}
	if !onDisk {
		f.Warnings = append(f.Warnings, "Not found on disk in Staging/")
	}
	switch n := f.ClockWarnings; {
	case n == 1:
		f.Warnings = append(f.Warnings, "1 file has a suspicious capture time")
	case n > 1:
		f.Warnings = append(f.Warnings, fmt.Sprintf("%d files have a suspicious capture time", n))
	}
	return f
}

func kindRank(k media.Kind) int {
	switch k {
	case media.KindImage:
		return 0
	case media.KindRaw:
		return 1
	}
	return 2
}

func stem(rel string) string {
	b := path.Base(rel)
	return strings.TrimSuffix(b, path.Ext(b))
}

// pickSamples picks up to n shots spread evenly across the sorted files. A
// shot is a run of files with the same base name apart from the extension
// (a RAW+JPEG pair); it is shown by its best file, JPEG first.
func pickSamples(sorted []catalog.Asset, n int) []Sample {
	var shots []catalog.Asset
	for i, a := range sorted {
		if i > 0 && stem(sorted[i-1].RelPath) == stem(a.RelPath) {
			if last := len(shots) - 1; kindRank(a.Kind) < kindRank(shots[last].Kind) {
				shots[last] = a
			}
			continue
		}
		shots = append(shots, a)
	}
	out := []Sample{}
	m := min(n, len(shots))
	for i := range m {
		a := shots[i*len(shots)/m]
		out = append(out, Sample{SHA256: a.SHA256, Kind: KindName(a.Kind)})
	}
	return out
}

func warnedFiles(ws []catalog.ClockWarning) map[string][]string {
	out := map[string][]string{}
	for _, w := range ws {
		out[w.SHA256] = append(out[w.SHA256], w.Detail)
	}
	return out
}

// Files returns folder id with its files, optionally only those of kind
// (jpg, raw or video). Folder always summarises every file.
func (s *Service) Files(ctx context.Context, id int64, kind string) (FolderFiles, error) {
	switch kind {
	case "", "jpg", "raw", "video":
	default:
		return FolderFiles{}, fmt.Errorf("%w, not %q", ErrBadKind, kind)
	}
	row, err := s.db.StagingFolderByID(ctx, id)
	if err != nil {
		return FolderFiles{}, err
	}
	if row.Name == "" || row.Name == "." || row.Name == ".." || strings.ContainsAny(row.Name, `/\`) {
		return FolderFiles{}, fmt.Errorf("staging folder %d has a name that is not a single folder name: %q", id, row.Name)
	}
	prefix := Dir + "/" + row.Name + "/"
	assets, err := s.db.AssetsUnder(ctx, prefix, catalog.StateStaged)
	if err != nil {
		return FolderFiles{}, err
	}
	ws, err := s.db.ClockWarningsUnder(ctx, prefix, catalog.StateStaged)
	if err != nil {
		return FolderFiles{}, err
	}
	onDisk := true
	if _, err := os.Stat(filepath.Join(s.photosRoot, Dir, row.Name)); errors.Is(err, fs.ErrNotExist) {
		onDisk = false
	} else if err != nil {
		return FolderFiles{}, fmt.Errorf("stat %s/%s: %w", Dir, row.Name, err)
	}
	sorted := sortFiles(assets)
	warned := warnedFiles(ws)
	out := FolderFiles{Folder: summarize(row, sorted, warned, onDisk), Files: []File{}}
	for _, a := range sorted {
		if kind != "" && KindName(a.Kind) != kind {
			continue
		}
		f := File{
			SHA256: a.SHA256, Name: path.Base(a.RelPath), Path: strings.TrimPrefix(a.RelPath, prefix),
			Kind: KindName(a.Kind), Size: a.Size, TZOffset: a.TZOffset, ClockWarnings: warned[a.SHA256],
		}
		if !a.CaptureTime.IsZero() {
			f.CaptureTime = a.CaptureTime.Format(catalog.CaptureLayout)
		}
		out.Files = append(out.Files, f)
	}
	return out, nil
}
