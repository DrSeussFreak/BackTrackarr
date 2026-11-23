package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Durable job kinds. All of them can safely repeat work, because imports
// recognize what's already stored.
const (
	jobKindFiles      = "files"
	jobKindEverything = "everything"
	jobKindPlex       = "plex-history"
	jobKindSource     = "source"
	jobKindTrakt      = "trakt-history"
)

func (e *Engine) registerJobRunners() {
	r := e.Jobs.runners
	r[jobKindFiles] = e.runFileJob
	r[jobKindEverything] = func(ctx context.Context, run *JobRun) error { return e.ImportEverything(ctx, run.Report) }
	r[jobKindPlex] = func(ctx context.Context, run *JobRun) error { return e.ImportPlex(ctx, run.Report) }
	r[jobKindSource] = func(ctx context.Context, run *JobRun) error {
		var name string
		if err := json.Unmarshal(run.Data, &name); err != nil {
			return err
		}
		return e.PullHistory(ctx, name, run.Report)
	}
	r[jobKindTrakt] = func(ctx context.Context, run *JobRun) error {
		var user string
		if err := json.Unmarshal(run.Data, &user); err != nil {
			return err
		}
		return e.ImportTrakt(ctx, user, run.Report)
	}
}

// ResumeJobs continues whatever was running before this start.
func (e *Engine) ResumeJobs(ctx context.Context) error { return e.Jobs.Resume(ctx) }

// StartImportEverything queues a read of every connected app.
func (e *Engine) StartImportEverything() (bool, error) {
	return e.Jobs.StartDurable(jobKindEverything, "import ← everything connected", nil, nil, true)
}

// StartPlexImport queues the Plex history import.
func (e *Engine) StartPlexImport() (bool, error) {
	return e.Jobs.StartDurable(jobKindPlex, "import ← plex", nil, nil, true)
}

// StartSourceImport queues one connected app's history.
func (e *Engine) StartSourceImport(name string) (bool, error) {
	return e.Jobs.StartDurable(jobKindSource, "import ← "+name, name, nil, true)
}

// StartTraktImport queues the connected Trakt account's history.
func (e *Engine) StartTraktImport(user string) (bool, error) {
	return e.Jobs.StartDurable(jobKindTrakt, "import ← trakt", user, nil, true)
}

// FileImport says how a batch of uploaded files is read.
type FileImport struct {
	Mode      string       `json:"mode"` // import, comics, books, games or streaming
	User      string       `json:"user,omitempty"`
	MediaType string       `json:"media_type,omitempty"`
	Stream    StreamImport `json:"stream,omitempty"`
}

// StartFileImport queues uploaded files. They move into the job, so the
// caller's copies are gone once this succeeds.
func (e *Engine) StartFileImport(name string, files []ImportFile, p FileImport) error {
	_, err := e.Jobs.StartDurable(jobKindFiles, name, p, files, false)
	return err
}

type fileCheckpoint struct {
	Next     int            `json:"next"`
	Totals   map[string]int `json:"totals"`
	Failed   []string       `json:"failed,omitempty"`
	Empty    int            `json:"empty,omitempty"`
	Formats  []string       `json:"formats,omitempty"`
	Profiles []string       `json:"profiles,omitempty"`
}

func addUnique(list []string, vals ...string) []string {
	for _, v := range vals {
		found := false
		for _, x := range list {
			found = found || x == v
		}
		if !found && v != "" {
			list = append(list, v)
		}
	}
	sort.Strings(list)
	return list
}

func (e *Engine) runFileJob(ctx context.Context, run *JobRun) error {
	var p FileImport
	if err := json.Unmarshal(run.Data, &p); err != nil {
		return err
	}
	if p.Mode == "import" && p.User == "" {
		p.User = e.defaultUser(ctx) // once, not a Plex lookup per file
	}
	ck := fileCheckpoint{Totals: map[string]int{}}
	if err := run.Load(&ck); err != nil {
		return err
	}
	if ck.Totals == nil {
		ck.Totals = map[string]int{}
	}
	n := len(run.Files)
	for i := ck.Next; i < n; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		f := run.Files[i]
		if n > 1 {
			run.Report(fmt.Sprintf("file %d of %d: %s. %s", i+1, n, f.Name, fileSummary(p.Mode, &ck)))
		} else {
			run.Report("reading " + f.Name)
		}
		err := e.importOne(ctx, p, f, &ck, run.Report)
		if ctx.Err() != nil {
			return ctx.Err() // this file runs again on resume
		}
		switch {
		case err == nil:
		case errors.Is(err, errNothingReadable) && n > 1:
			ck.Empty++
		default:
			ck.Failed = append(ck.Failed, f.Name+": "+err.Error())
		}
		ck.Next = i + 1
		if err := run.Save(ck, ck.Next, ck.Next == n); err != nil && e.log != nil {
			e.log.Warn("job progress not saved", "job", run.job.Name, "err", err)
		}
	}
	run.Report(fileSummary(p.Mode, &ck))
	if n > 1 && ck.Empty == n {
		return errNothingReadable
	}
	if len(ck.Failed) == 0 {
		return nil
	}
	shown, more := ck.Failed, ""
	if len(shown) > 5 {
		more = fmt.Sprintf(" and %d more", len(shown)-5)
		shown = shown[:5]
	}
	return errors.New(strings.Join(shown, "; ") + more)
}

func (e *Engine) importOne(ctx context.Context, p FileImport, u ImportFile, ck *fileCheckpoint, report func(string)) error {
	t := ck.Totals
	universal := func() error {
		res, err := e.ImportFiles(ctx, []ImportFile{u}, p.User, func(string) {})
		if res != nil && ctx.Err() == nil {
			t["plays"] += res.Plays
			t["duplicates"] += res.Duplicates
			t["ratings"] += res.Ratings
			t["kept"] += res.Kept
			t["shelf"] += res.Shelf
			t["comics"] += res.Comics
			t["skipped"] += res.Skipped
			ck.Formats = addUnique(ck.Formats, res.Formats...)
		}
		return err
	}
	isJSON := strings.EqualFold(filepath.Ext(u.Name), ".json")
	if p.Mode == "import" || (isJSON && (p.Mode == "comics" || p.Mode == "books")) {
		return universal()
	}
	f, err := os.Open(u.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	switch p.Mode {
	case "comics":
		res, err := e.ImportCLZProgress(ctx, f, "clz:"+u.Name, func(string) {})
		if res != nil && ctx.Err() == nil {
			t["rows"] += res.Rows
			t["new"] += res.New
			t["updated"] += res.Updated
			t["reads"] += res.Reads
		}
		return err
	case "books", "games":
		var res *ShelfImportResult
		if p.Mode == "games" {
			res, err = e.ImportGames(ctx, f)
		} else {
			res, err = e.ImportBooks(ctx, f, p.MediaType)
		}
		if res != nil && ctx.Err() == nil {
			t["rows"] += res.Rows
			t["new"] += res.New
			t["updated"] += res.Updated
			t["reads"] += res.Events
		}
		return err
	case "streaming":
		res, err := e.ImportStreaming(ctx, f, p.Stream, report)
		if err != nil {
			return err
		}
		t["rows"] += res.Rows
		t["plays"] += res.Added
		t["duplicates"] += res.Duplicates
		t["skipped"] += res.Skipped
		t["unmatched"] += res.Unmatched
		ck.Formats = addUnique(ck.Formats, res.Service)
		if p.Stream.Profile == "" && p.Stream.User == "" {
			ck.Profiles = addUnique(ck.Profiles, res.Profiles...)
		}
		return nil
	}
	return fmt.Errorf("unknown import type %q", p.Mode)
}

func fileSummary(mode string, ck *fileCheckpoint) string {
	t := ck.Totals
	universal := (&UniversalResult{Plays: t["plays"], Duplicates: t["duplicates"], Ratings: t["ratings"], Kept: t["kept"],
		Shelf: t["shelf"], Comics: t["comics"], Skipped: t["skipped"], Formats: ck.Formats}).String()
	var s string
	switch mode {
	case "comics":
		s = fmt.Sprintf("%d comics, %d new, %d updated, %d new reads", t["rows"], t["new"], t["updated"], t["reads"])
	case "books", "games":
		s = fmt.Sprintf("%d rows, %d new, %d updated, %d new reads or plays", t["rows"], t["new"], t["updated"], t["reads"])
	case "streaming":
		s = fmt.Sprintf("%s: %d rows, %d new plays, %d already there, %d skipped (trailers, previews, unreadable)",
			strings.Join(ck.Formats, ", "), t["rows"], t["plays"], t["duplicates"], t["skipped"])
		if t["unmatched"] > 0 {
			s += fmt.Sprintf(", %d without ids", t["unmatched"])
		}
		if len(ck.Profiles) > 1 {
			s += "; profiles were kept as users: " + strings.Join(ck.Profiles, ", ")
		}
		return s
	default:
		return universal
	}
	if t["plays"]+t["duplicates"]+t["ratings"]+t["kept"] > 0 {
		s += "; from JSON: " + universal
	}
	return s
}
