package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"
)

// StagingFolder is a directory under Staging/ and the capture dates it covers.
type StagingFolder struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Dates     []string  `json:"dates"` // sorted YYYY-MM-DD
	CreatedAt time.Time `json:"created_at"`
}

var dateRE = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

const stagingCols = "id, name, dates, created_at"

func scanStagingFolder(s scanner) (StagingFolder, error) {
	var (
		f              StagingFolder
		dates, created string
	)
	if err := s.Scan(&f.ID, &f.Name, &dates, &created); err != nil {
		return StagingFolder{}, err
	}
	if err := json.Unmarshal([]byte(dates), &f.Dates); err != nil {
		return StagingFolder{}, fmt.Errorf("staging folder %q: decode dates: %w", f.Name, err)
	}
	t, err := parseTime(created)
	if err != nil {
		return StagingFolder{}, fmt.Errorf("staging folder %q: parse created_at: %w", f.Name, err)
	}
	f.CreatedAt = t
	return f, nil
}

// StagingFolderForDate returns the folder ingest files a capture on date
// (YYYY-MM-DD) into, creating or extending a row as Decision 12 says.
func (d *DB) StagingFolderForDate(ctx context.Context, date string, at time.Time) (StagingFolder, error) {
	if !dateRE.MatchString(date) {
		return StagingFolder{}, fmt.Errorf("staging folder: bad date %q", date)
	}
	var out StagingFolder
	err := d.tx(ctx, func(tx *sql.Tx) error {
		f, err := scanStagingFolder(tx.QueryRowContext(ctx, "SELECT "+stagingCols+` FROM staging_folders f
			WHERE EXISTS (SELECT 1 FROM json_each(f.dates) WHERE json_each.value = ?) ORDER BY id LIMIT 1`, date))
		if err == nil {
			out = f
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		f, err = scanStagingFolder(tx.QueryRowContext(ctx, "SELECT "+stagingCols+" FROM staging_folders WHERE name = ?", date))
		if err == nil {
			f.Dates = append(f.Dates, date)
			slices.Sort(f.Dates)
			b, err := json.Marshal(f.Dates)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE staging_folders SET dates = ? WHERE id = ?", string(b), f.ID); err != nil {
				return err
			}
			out = f
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		res, err := tx.ExecContext(ctx, "INSERT INTO staging_folders (name, dates, created_at) VALUES (?,?,?)",
			date, `["`+date+`"]`, formatTime(at))
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		out = StagingFolder{ID: id, Name: date, Dates: []string{date}, CreatedAt: at.UTC().Truncate(time.Millisecond)}
		return nil
	})
	if err != nil {
		return StagingFolder{}, fmt.Errorf("staging folder for %s: %w", date, err)
	}
	return out, nil
}

// StagingFolders returns every folder ordered by name.
func (d *DB) StagingFolders(ctx context.Context) ([]StagingFolder, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT "+stagingCols+" FROM staging_folders ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("staging folders: %w", err)
	}
	var out []StagingFolder
	for rows.Next() {
		f, err := scanStagingFolder(rows)
		if err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Join(err, rows.Close())
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, nil
}
