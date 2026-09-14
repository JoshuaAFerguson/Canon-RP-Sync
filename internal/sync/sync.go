// Package sync computes what needs importing and drives the transfer.
//
// Every import goes through the same three steps regardless of whether the
// bytes came from the camera over Wi-Fi or from a phone that shot on the
// camera's own hotspot:
//
//  1. stage — write to a temporary file inside the destination volume
//  2. inspect — hash the bytes and read the capture date
//  3. place — move into dest/<layout>/ and record it in the manifest
//
// Staging first is what makes capture-date folders possible: the date is only
// known once the file is on disk, and a half-transferred file never looks like
// a finished import.
package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/JoshuaAFerguson/canon-rp-sync/internal/ccapi"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/exif"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/manifest"
)

// stagingDir holds partially transferred files. It lives inside the
// destination so the final move is a rename rather than a copy.
const stagingDir = ".rpsync-incoming"

// maxConsecutiveErrors stops a run that is failing repeatedly, rather than
// walking the whole card retrying a camera that has gone away.
const maxConsecutiveErrors = 5

// DateSource selects which timestamp decides the destination folder.
type DateSource string

const (
	// DateFromEXIF uses the capture time recorded in the file, falling back to
	// what the camera reports and finally to the import time.
	DateFromEXIF DateSource = "exif"
	// DateFromImport uses the time the file was imported.
	DateFromImport DateSource = "import"
)

// Options controls an importer.
type Options struct {
	// Dest is the root directory imports are written under.
	Dest string
	// Layout is a Go time layout describing the folders under Dest, e.g.
	// "2006/2006-01-02".
	Layout string
	// Include limits imports to these lower-case extensions. Empty means all.
	Include map[string]bool
	// DeleteAfterImport removes the file from the card once it is safely
	// written. Off unless the user explicitly opts in.
	DeleteAfterImport bool
	// DateSource selects the folder date.
	DateSource DateSource
}

func (o *Options) setDefaults() {
	if o.Layout == "" {
		o.Layout = "2006/2006-01-02"
	}
	if o.DateSource == "" {
		o.DateSource = DateFromEXIF
	}
}

// Progress describes one step of a run, for logs and the live event stream.
type Progress struct {
	// Phase is "listing", "importing", "imported", "skipped" or "error".
	Phase string
	File  string
	Entry *manifest.Entry
	Err   error
	// Done and Total are populated during "importing".
	Done, Total int
}

// Result summarises a run.
type Result struct {
	Imported int
	Skipped  int
	Bytes    int64
	Entries  []manifest.Entry
	Errors   []error
}

// Importer imports files into a destination tree, recording them in a manifest.
type Importer struct {
	opt Options
	mf  *manifest.Manifest
	// Notify, if set, is called as the run progresses. It must not block.
	Notify func(Progress)
}

// New returns an importer writing into opt.Dest and recording into mf.
func New(mf *manifest.Manifest, opt Options) *Importer {
	opt.setDefaults()
	return &Importer{opt: opt, mf: mf}
}

// Options reports the importer's configuration.
func (im *Importer) Options() Options { return im.opt }

func (im *Importer) notify(p Progress) {
	if im.Notify != nil {
		im.Notify(p)
	}
}

// Pull imports every file on the camera that is not yet in the manifest.
func (im *Importer) Pull(ctx context.Context, cam *ccapi.Client) (Result, error) {
	im.notify(Progress{Phase: "listing"})

	files, err := cam.ListFiles(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("list files: %w", err)
	}
	return im.PullFiles(ctx, cam, files)
}

// PullFiles imports a specific set of camera files, e.g. the ones named by an
// event poll.
func (im *Importer) PullFiles(ctx context.Context, cam *ccapi.Client, files []ccapi.File) (Result, error) {
	var (
		res         Result
		consecutive int
	)

	pending := make([]ccapi.File, 0, len(files))
	for _, f := range files {
		if !im.wanted(f) {
			res.Skipped++
			continue
		}
		pending = append(pending, f)
	}

	for i, f := range pending {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		im.notify(Progress{Phase: "importing", File: f.Name, Done: i, Total: len(pending)})

		entry, err := im.importOne(ctx, cam, f)
		if err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			err = fmt.Errorf("import %s: %w", f.Name, err)
			res.Errors = append(res.Errors, err)
			im.notify(Progress{Phase: "error", File: f.Name, Err: err})

			consecutive++
			if consecutive >= maxConsecutiveErrors {
				return res, fmt.Errorf("giving up after %d consecutive failures: %w", consecutive, err)
			}
			continue
		}
		consecutive = 0
		res.Imported++
		res.Bytes += entry.Size
		res.Entries = append(res.Entries, entry)
		im.notify(Progress{Phase: "imported", File: f.Name, Entry: &entry, Done: i + 1, Total: len(pending)})
	}
	return res, nil
}

// wanted reports whether a camera file should be imported at all.
func (im *Importer) wanted(f ccapi.File) bool {
	if len(im.opt.Include) > 0 && !im.opt.Include[f.Ext()] {
		return false
	}
	return !im.mf.Has(f.Card, f.Dir, f.Name)
}

// importOne stages, inspects and places a single camera file.
func (im *Importer) importOne(ctx context.Context, cam *ccapi.Client, f ccapi.File) (manifest.Entry, error) {
	staged, err := im.stagingPath(f.Name)
	if err != nil {
		return manifest.Entry{}, err
	}
	defer os.Remove(staged)

	size, err := cam.Download(ctx, f, staged)
	if err != nil {
		return manifest.Entry{}, err
	}

	sum, err := hashFile(staged)
	if err != nil {
		return manifest.Entry{}, err
	}

	captured := im.captureTime(ctx, cam, f, staged)

	dest, err := im.place(staged, f.Name, captured, sum)
	if err != nil {
		return manifest.Entry{}, err
	}

	entry := manifest.Entry{
		ID:         manifest.IDFor(f.Card, f.Dir, f.Name),
		Card:       f.Card,
		Dir:        f.Dir,
		Name:       f.Name,
		Dest:       dest,
		Ext:        f.Ext(),
		Size:       size,
		SHA256:     sum,
		Source:     manifest.SourceCamera,
		CapturedAt: captured,
		ImportedAt: time.Now(),
	}
	if err := im.mf.Add(entry); err != nil {
		return entry, fmt.Errorf("record in manifest: %w", err)
	}

	if im.opt.DeleteAfterImport {
		if err := cam.Delete(ctx, f); err != nil {
			// The import succeeded; a failed cleanup must not undo it.
			im.notify(Progress{Phase: "error", File: f.Name,
				Err: fmt.Errorf("delete %s from card: %w", f.Name, err)})
		}
	}
	return entry, nil
}

// IngestMeta describes a file handed over by a companion app.
type IngestMeta struct {
	// Name is the original file name, e.g. IMG_0001.CR3.
	Name string
	// Card and Dir identify the file on the camera when the app knows them, so
	// the daemon will not pull the same shot again over Wi-Fi.
	Card string
	Dir  string
	// CapturedAt is the app's idea of the capture time. EXIF in the uploaded
	// bytes wins when it disagrees.
	CapturedAt time.Time
	// Device names the paired device, recorded as the entry's source.
	Device string
}

// Ingest imports a file uploaded by a companion app. Uploads are deduplicated
// by content hash as well as by camera-side identity, so handing over the same
// shot twice — or handing over a shot the daemon already pulled — is a no-op.
func (im *Importer) Ingest(ctx context.Context, r io.Reader, meta IngestMeta) (manifest.Entry, bool, error) {
	if meta.Name == "" {
		return manifest.Entry{}, false, errors.New("ingest: file name is required")
	}
	meta.Name = filepath.Base(filepath.Clean(meta.Name))
	if meta.Card == "" {
		meta.Card = "upload"
	}
	if meta.Dir == "" {
		meta.Dir = "device"
	}

	if existing, ok := im.mf.Get(meta.Card, meta.Dir, meta.Name); ok {
		return existing, false, nil
	}

	staged, err := im.stagingPath(meta.Name)
	if err != nil {
		return manifest.Entry{}, false, err
	}
	defer os.Remove(staged)

	size, sum, err := writeAndHash(staged, r)
	if err != nil {
		return manifest.Entry{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return manifest.Entry{}, false, err
	}

	// The same bytes under another name: already imported.
	if existing, ok := im.mf.ByHash(sum); ok {
		return existing, false, nil
	}

	captured := meta.CapturedAt
	if im.opt.DateSource == DateFromEXIF {
		if md, err := exif.FromFile(staged); err == nil && !md.CapturedAt.IsZero() {
			captured = md.CapturedAt
		}
	}
	if captured.IsZero() {
		captured = time.Now()
	}

	dest, err := im.place(staged, meta.Name, captured, sum)
	if err != nil {
		return manifest.Entry{}, false, err
	}

	source := manifest.SourceCamera
	if meta.Device != "" {
		source = manifest.SourceDevicePrefix + meta.Device
	}
	entry := manifest.Entry{
		ID:         manifest.IDFor(meta.Card, meta.Dir, meta.Name),
		Card:       meta.Card,
		Dir:        meta.Dir,
		Name:       meta.Name,
		Dest:       dest,
		Ext:        strings.ToLower(strings.TrimPrefix(path.Ext(meta.Name), ".")),
		Size:       size,
		SHA256:     sum,
		Source:     source,
		CapturedAt: captured,
		ImportedAt: time.Now(),
	}
	if err := im.mf.Add(entry); err != nil {
		return entry, true, fmt.Errorf("record in manifest: %w", err)
	}
	im.notify(Progress{Phase: "imported", File: meta.Name, Entry: &entry})
	return entry, true, nil
}

// captureTime resolves the timestamp used to file the photo.
func (im *Importer) captureTime(ctx context.Context, cam *ccapi.Client, f ccapi.File, staged string) time.Time {
	if im.opt.DateSource == DateFromImport {
		return time.Now()
	}
	if md, err := exif.FromFile(staged); err == nil && !md.CapturedAt.IsZero() {
		return md.CapturedAt
	}
	// Not every file carries readable metadata; ask the camera before giving up.
	if info, err := cam.FileInfo(ctx, f); err == nil && !info.CapturedAt.IsZero() {
		return info.CapturedAt
	}
	return time.Now()
}

// place moves a staged file to its final home, resolving name collisions.
func (im *Importer) place(staged, name string, captured time.Time, sum string) (string, error) {
	dir := filepath.Join(im.opt.Dest, filepath.FromSlash(captured.Format(im.opt.Layout)))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	dest := filepath.Join(dir, name)
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)

	for i := 0; ; i++ {
		if i > 0 {
			dest = filepath.Join(dir, fmt.Sprintf("%s_%d%s", stem, i, ext))
		}
		existing, err := os.Stat(dest)
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			return "", err
		}
		// A file is already there. If it is the same photo, keep it and drop
		// the staged copy; otherwise pick the next free name.
		if existing.Size() > 0 {
			if have, err := hashFile(dest); err == nil && have == sum {
				return dest, nil
			}
		}
		if i > 1000 {
			return "", fmt.Errorf("cannot find a free name for %s in %s", name, dir)
		}
	}

	if err := os.Rename(staged, dest); err != nil {
		return "", fmt.Errorf("move into place: %w", err)
	}
	return dest, nil
}

// stagingPath returns a unique temporary path inside the destination volume.
func (im *Importer) stagingPath(name string) (string, error) {
	dir := filepath.Join(im.opt.Dest, stagingDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "*-"+filepath.Base(name))
	if err != nil {
		return "", err
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		return "", err
	}
	// The downloader creates the file itself; it only needs the name.
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeAndHash streams r to path, returning the byte count and content hash.
func writeAndHash(path string, r io.Reader) (int64, string, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return n, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// CleanStaging removes leftover partial transfers, e.g. after a crash.
func CleanStaging(dest string) error {
	dir := filepath.Join(dest, stagingDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
