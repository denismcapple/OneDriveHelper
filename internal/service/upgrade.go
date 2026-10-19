package service

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// fileOps is the file system as the upgrade sees it (replaced in tests).
type fileOps struct {
	copy   func(src, dst string) error // copy and flush to disk
	rename func(from, to string) error
	remove func(path string) error
	exists func(path string) bool
	glob   func(pattern string) ([]string, error)
	now    func() time.Time
}

func realFileOps() fileOps {
	return fileOps{
		copy:   copyFlush,
		rename: os.Rename, // MoveFileEx with REPLACE_EXISTING
		remove: os.Remove,
		exists: func(p string) bool { _, err := os.Lstat(p); return err == nil },
		glob:   filepath.Glob,
		now:    time.Now,
	}
}

func copyFlush(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// swapped records one installed file that was replaced.
type swapped struct {
	name, old string // old is "" when there was no previous file
}

// upgrade replaces the programs in dir with those in srcDir so that a
// failure at any point leaves the previous version in place:
//
//  1. remove stale *.new / *.old-* files from earlier runs (in-use ones are ignored)
//  2. copy every program to <name>.new and flush it, before touching any installed file
//  3. swap each: <name> -> <name>.old-<timestamp> (a running image can be renamed
//     but not overwritten), then <name>.new -> <name>
//
// On failure everything swapped so far is renamed back. The returned rollback
// does the same for failures after the swap (e.g. the service not starting).
func upgrade(srcDir, dir string, names []string, o fileOps) (rollback func() error, err error) {
	cleanStale(dir, o)
	var present []string
	for _, n := range names {
		if o.exists(filepath.Join(srcDir, n)) {
			present = append(present, n)
		}
	}
	// 2. stage
	for i, n := range present {
		if err := o.copy(filepath.Join(srcDir, n), filepath.Join(dir, n+".new")); err != nil {
			for _, s := range present[:i+1] {
				_ = o.remove(filepath.Join(dir, s+".new"))
			}
			return nil, fmt.Errorf("stage %s: %w", n, err)
		}
	}
	// 3. swap
	stamp := o.now().Format("20060102-150405.000")
	var done []swapped
	undo := func() error {
		var first error
		for i := len(done) - 1; i >= 0; i-- {
			s := done[i]
			cur := filepath.Join(dir, s.name)
			if s.old == "" {
				if err := o.remove(cur); err != nil && first == nil {
					first = err
				}
				continue
			}
			// Move the new file out of the way (it may be in use too), then put the old one back.
			_ = o.rename(cur, cur+".failed-"+stamp)
			if err := o.rename(s.old, cur); err != nil && first == nil {
				first = fmt.Errorf("restore %s: %w", s.name, err)
			}
		}
		return first
	}
	for _, n := range present {
		cur := filepath.Join(dir, n)
		s := swapped{name: n}
		if o.exists(cur) {
			s.old = cur + ".old-" + stamp
			if err := o.rename(cur, s.old); err != nil {
				uerr := undo()
				removeNew(dir, present, o)
				return nil, joinErr(fmt.Errorf("move aside %s: %w", n, err), uerr)
			}
		}
		if err := o.rename(cur+".new", cur); err != nil {
			if s.old != "" {
				_ = o.rename(s.old, cur)
			}
			uerr := undo()
			removeNew(dir, present, o)
			return nil, joinErr(fmt.Errorf("install %s: %w", n, err), uerr)
		}
		done = append(done, s)
	}
	return undo, nil
}

func removeNew(dir string, names []string, o fileOps) {
	for _, n := range names {
		_ = o.remove(filepath.Join(dir, n+".new"))
	}
}

// cleanStale removes leftovers of earlier upgrades; files still in use stay
// (their unique names never block a later swap).
func cleanStale(dir string, o fileOps) {
	for _, pat := range []string{"*.new", "*.old", "*.old-*", "*.failed-*"} {
		files, _ := o.glob(filepath.Join(dir, pat))
		for _, f := range files {
			_ = o.remove(f)
		}
	}
}

func joinErr(err, rollbackErr error) error {
	if rollbackErr != nil {
		return fmt.Errorf("%w (and rolling back failed: %v)", err, rollbackErr)
	}
	return err
}
