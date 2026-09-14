// Package sync computes what needs importing and drives the download.
package sync

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/JoshuaAFerguson/canon-rp-sync/internal/ccapi"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/manifest"
)

// Options controls a sync run.
type Options struct {
	Dest              string
	Include           map[string]bool // lower-case extensions; empty = everything
	DeleteAfterImport bool
}

// Run imports every file on the camera that is not yet in the manifest.
func Run(ctx context.Context, cam *ccapi.Client, mf *manifest.Manifest, opt Options) (int, error) {
	files, err := cam.ListFiles(ctx)
	if err != nil {
		return 0, fmt.Errorf("list files: %w", err)
	}

	imported := 0
	for _, f := range files {
		if len(opt.Include) > 0 && !opt.Include[f.Ext()] {
			continue
		}
		if mf.Has(f.Card, f.Dir, f.Name) {
			continue
		}
		dest := destPath(opt.Dest, f)
		log.Printf("importing %s -> %s", f.Name, dest)
		if err := cam.Download(ctx, f, dest); err != nil {
			return imported, fmt.Errorf("download %s: %w", f.Name, err)
		}
		if err := mf.Add(manifest.Entry{Card: f.Card, Dir: f.Dir, Name: f.Name, Dest: dest}); err != nil {
			return imported, fmt.Errorf("manifest: %w", err)
		}
		if opt.DeleteAfterImport {
			if err := cam.Delete(ctx, f); err != nil {
				log.Printf("warning: delete %s failed: %v", f.Name, err)
			}
		}
		imported++
	}
	return imported, nil
}

// destPath places files under dest/YYYY/YYYY-MM-DD/. Capture date will come
// from EXIF once we parse it; for now it's the import date.
// TODO: read DateTimeOriginal from the file (JPEG EXIF / CR3 CMT1 box).
func destPath(root string, f ccapi.File) string {
	now := time.Now()
	return filepath.Join(root, now.Format("2006"), now.Format("2006-01-02"), f.Name)
}
