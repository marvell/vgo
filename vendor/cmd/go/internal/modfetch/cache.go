// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package modfetch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"cmd/go/internal/modfetch/codehost"
	"cmd/go/internal/par"
	"cmd/go/internal/semver"
)

var QuietLookup bool // do not print about lookups

var SrcMod string // $GOPATH/src/mod; set by package vgo

// A cachingRepo is a cache around an underlying Repo,
// avoiding redundant calls to ModulePath, Versions, Stat, Latest, and GoMod (but not Zip).
// It is also safe for simultaneous use by multiple goroutines
// (so that it can be returned from Lookup multiple times).
// It serializes calls to the underlying Repo.
type cachingRepo struct {
	path  string
	cache par.Cache // cache for all operations
	r     Repo
}

func newCachingRepo(r Repo) *cachingRepo {
	return &cachingRepo{
		r:    r,
		path: r.ModulePath(),
	}
}

func (r *cachingRepo) ModulePath() string {
	return r.path
}

func (r *cachingRepo) Versions(prefix string) ([]string, error) {
	type cached struct {
		list []string
		err  error
	}
	c := r.cache.Do("versions:"+prefix, func() interface{} {
		list, err := r.r.Versions(prefix)
		return cached{list, err}
	}).(cached)

	if c.err != nil {
		return nil, c.err
	}
	return append([]string(nil), c.list...), nil
}

type cachedInfo struct {
	info *RevInfo
	err  error
}

func (r *cachingRepo) Stat(rev string) (*RevInfo, error) {
	c := r.cache.Do("stat:"+rev, func() interface{} {
		file, info, err := readDiskStat(r.path, rev)
		if err == nil {
			return cachedInfo{info, nil}
		}

		if !QuietLookup {
			fmt.Fprintf(os.Stderr, "vgo: finding %s %s\n", r.path, rev)
		}
		info, err = r.r.Stat(rev)
		if err == nil {
			if err := writeDiskStat(file, info); err != nil {
				fmt.Fprintf(os.Stderr, "go: writing stat cache: %v\n", err)
			}
			// If we resolved, say, 1234abcde to v0.0.0-20180604122334-1234abcdef78,
			// then save the information under the proper version, for future use.
			if info.Version != rev {
				r.cache.Do("stat:"+info.Version, func() interface{} {
					return cachedInfo{info, err}
				})
			}
		}
		return cachedInfo{info, err}
	}).(cachedInfo)

	if c.err != nil {
		return nil, c.err
	}
	info := *c.info
	return &info, nil
}

func (r *cachingRepo) Latest() (*RevInfo, error) {
	c := r.cache.Do("latest:", func() interface{} {
		if !QuietLookup {
			fmt.Fprintf(os.Stderr, "vgo: finding %s latest\n", r.path)
		}
		info, err := r.r.Latest()

		// Save info for likely future Stat call.
		if err == nil {
			r.cache.Do("stat:"+info.Version, func() interface{} {
				return cachedInfo{info, err}
			})
			if file, _, err := readDiskStat(r.path, info.Version); err != nil {
				writeDiskStat(file, info)
			}
		}

		return cachedInfo{info, err}
	}).(cachedInfo)

	if c.err != nil {
		return nil, c.err
	}
	info := *c.info
	return &info, nil
}

func (r *cachingRepo) GoMod(rev string) ([]byte, error) {
	type cached struct {
		text []byte
		err  error
	}
	c := r.cache.Do("gomod:"+rev, func() interface{} {
		file, text, err := readDiskGoMod(r.path, rev)
		if err == nil {
			// Note: readDiskGoMod already called checkGoMod.
			return cached{text, nil}
		}

		// Convert rev to canonical version
		// so that we use the right identifier in the go.sum check.
		info, err := r.Stat(rev)
		if err != nil {
			return cached{nil, err}
		}
		rev = info.Version

		text, err = r.r.GoMod(rev)
		checkGoMod(r.path, rev, text)
		if err == nil {
			if err := writeDiskGoMod(file, text); err != nil {
				fmt.Fprintf(os.Stderr, "go: writing go.mod cache: %v\n", err)
			}
		}
		return cached{text, err}
	}).(cached)

	if c.err != nil {
		return nil, c.err
	}
	return append([]byte(nil), c.text...), nil
}

func (r *cachingRepo) Zip(version, tmpdir string) (string, error) {
	return r.r.Zip(version, tmpdir)
}

// Stat is like Lookup(path).Stat(rev) but avoids the
// repository path resolution in Lookup if the result is
// already cached on local disk.
func Stat(path, rev string) (*RevInfo, error) {
	_, info, err := readDiskStat(path, rev)
	if err == nil {
		return info, nil
	}
	repo, err := Lookup(path)
	if err != nil {
		return nil, err
	}
	return repo.Stat(rev)
}

// GoMod is like Lookup(path).GoMod(rev) but avoids the
// repository path resolution in Lookup if the result is
// already cached on local disk.
func GoMod(path, rev string) ([]byte, error) {
	// Convert commit hash to pseudo-version
	// to increase cache hit rate.
	if !semver.IsValid(rev) {
		info, err := Stat(path, rev)
		if err != nil {
			return nil, err
		}
		rev = info.Version
	}
	_, data, err := readDiskGoMod(path, rev)
	if err == nil {
		return data, nil
	}
	repo, err := Lookup(path)
	if err != nil {
		return nil, err
	}
	return repo.GoMod(rev)
}

var errNotCached = fmt.Errorf("not in cache")

// readDiskStat reads a cached stat result from disk,
// returning the name of the cache file and the result.
// If the read fails, the caller can use
// writeDiskStat(file, info) to write a new cache entry.
func readDiskStat(path, rev string) (file string, info *RevInfo, err error) {
	file, data, err := readDiskCache(path, rev, "info")
	if err != nil {
		if file, info, err := readDiskStatByHash(path, rev); err == nil {
			return file, info, nil
		}
		return file, nil, err
	}
	info = new(RevInfo)
	if err := json.Unmarshal(data, info); err != nil {
		return file, nil, errNotCached
	}
	return file, info, nil
}

// readDiskStatByHash is a fallback for readDiskStat for the case
// where rev is a commit hash instead of a proper semantic version.
// In that case, we look for a cached pseudo-version that matches
// the commit hash. If we find one, we use it.
// This matters most for converting legacy package management
// configs, when we are often looking up commits by full hash.
// Without this check we'd be doing network I/O to the remote repo
// just to find out about a commit we already know about
// (and have cached under its pseudo-version).
func readDiskStatByHash(path, rev string) (file string, info *RevInfo, err error) {
	if !codehost.AllHex(rev) || len(rev) < 12 {
		return "", nil, errNotCached
	}
	rev = rev[:12]
	dir, err := os.Open(filepath.Join(SrcMod, "cache/download", path, "@v"))
	if err != nil {
		return "", nil, errNotCached
	}
	names, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		return "", nil, errNotCached
	}
	suffix := "-" + rev + ".info"
	for _, name := range names {
		if strings.HasSuffix(name, suffix) && IsPseudoVersion(strings.TrimSuffix(name, ".info")) {
			return readDiskStat(path, strings.TrimSuffix(name, ".info"))
		}
	}
	return "", nil, errNotCached
}

// oldVgoPrefix is the prefix in the old auto-generated cached go.mod files.
// We stopped trying to auto-generate the go.mod files. Now we use a trivial
// go.mod with only a module line, and we've dropped the version prefix
// entirely. If we see a version prefix, that means we're looking at an old copy
// and should ignore it.
var oldVgoPrefix = []byte("//vgo 0.0.")

// readDiskGoMod reads a cached stat result from disk,
// returning the name of the cache file and the result.
// If the read fails, the caller can use
// writeDiskGoMod(file, data) to write a new cache entry.
func readDiskGoMod(path, rev string) (file string, data []byte, err error) {
	file, data, err = readDiskCache(path, rev, "mod")

	// If the file has an old auto-conversion prefix, pretend it's not there.
	if bytes.HasPrefix(data, oldVgoPrefix) {
		err = errNotCached
		data = nil
	}

	if err == nil {
		checkGoMod(path, rev, data)
	}

	return file, data, err
}

// readDiskCache is the generic "read from a cache file" implementation.
// It takes the revision and an identifying suffix for the kind of data being cached.
// It returns the name of the cache file and the content of the file.
// If the read fails, the caller can use
// writeDiskCache(file, data) to write a new cache entry.
func readDiskCache(path, rev, suffix string) (file string, data []byte, err error) {
	if !semver.IsValid(rev) || SrcMod == "" {
		return "", nil, errNotCached
	}
	file = filepath.Join(SrcMod, "cache/download", path, "@v", rev+"."+suffix)
	data, err = ioutil.ReadFile(file)
	if err != nil {
		return file, nil, errNotCached
	}
	return file, data, nil
}

// writeDiskStat writes a stat result cache entry.
// The file name must have been returned by a previous call to readDiskStat.
func writeDiskStat(file string, info *RevInfo) error {
	if file == "" {
		return nil
	}
	js, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return writeDiskCache(file, js)
}

// writeDiskGoMod writes a go.mod cache entry.
// The file name must have been returned by a previous call to readDiskGoMod.
func writeDiskGoMod(file string, text []byte) error {
	return writeDiskCache(file, text)
}

// writeDiskCache is the generic "write to a cache file" implementation.
// The file must have been returned by a previous call to readDiskCache.
func writeDiskCache(file string, data []byte) error {
	if file == "" {
		return nil
	}
	// Make sure directory for file exists.
	if err := os.MkdirAll(filepath.Dir(file), 0777); err != nil {
		return err
	}
	// Write data to temp file next to target file.
	f, err := ioutil.TempFile(filepath.Dir(file), filepath.Base(file)+".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Rename temp file onto cache file,
	// so that the cache file is always a complete file.
	return os.Rename(f.Name(), file)
}
