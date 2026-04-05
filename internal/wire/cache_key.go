// Copyright 2018 The Wire Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package wire

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"golang.org/x/tools/go/packages"
)

const maxParallelReads = 8

// sha256HasherPool reuses sha256 hashers to avoid per-call allocations.
// Initialized at package load; hashers are Reset before Put.
var sha256HasherPool = sync.Pool{
	New: func() any { return sha256.New() },
}

// getPooledSHA256 returns a hasher from the pool and a release function that
// Resets and returns it. Call release when done (e.g. defer release()).
func getPooledSHA256() (hash.Hash, func()) {
	h := sha256HasherPool.Get().(hash.Hash)
	h.Reset()
	return h, func() {
		h.Reset()
		sha256HasherPool.Put(h)
	}
}

// sumHex returns the hex-encoded hash digest without allocating an intermediate
// byte slice for the sum (uses a stack-allocated [sha256.Size]byte buffer).
func sumHex(h hash.Hash) string {
	var buf [sha256.Size]byte
	return hex.EncodeToString(h.Sum(buf[:0]))
}

// cacheVersion is the schema/version identifier for cache entries.
const cacheVersion = "wire-cache-v3"

// cacheFile captures file metadata used to validate cached content.
type cacheFile struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
}

// cacheMeta tracks inputs and outputs for a single package cache entry.
type cacheMeta struct {
	Version     string      `json:"version"`
	PkgPath     string      `json:"pkg_path"`
	Tags        string      `json:"tags"`
	Prefix      string      `json:"prefix"`
	HeaderHash  string      `json:"header_hash"`
	Files       []cacheFile `json:"files"`
	ContentHash string      `json:"content_hash"`
	RootHash    string      `json:"root_hash"`
}

// cacheKeyForPackage returns the content hash for a package, if cacheable.
func cacheKeyForPackage(pkg *packages.Package, opts *GenerateOptions) (string, error) {
	files := packageFiles(pkg)
	if len(files) == 0 {
		return "", nil
	}
	sort.Strings(files)
	metaKey := cacheMetaKey(pkg, opts)
	if meta, ok := readCacheMeta(metaKey); ok {
		if cacheMetaMatches(meta, pkg, opts, files) {
			return meta.ContentHash, nil
		}
	}
	contentHash, err := contentHashForFiles(pkg, opts, files)
	if err != nil {
		return "", err
	}
	rootFiles := rootPackageFiles(pkg)
	sort.Strings(rootFiles)
	var cache *FileHashCache
	if opts != nil {
		cache = opts.FileHashCache
	}
	rootHash, err := hashFiles(rootFiles, cache)
	if err != nil {
		return "", err
	}
	var statCache *FileStatCache
	if opts != nil {
		statCache = opts.FileStatCache
	}
	metaFiles, err := buildCacheFiles(files, statCache)
	if err != nil {
		return "", err
	}
	meta := &cacheMeta{
		Version:     cacheVersion,
		PkgPath:     pkg.PkgPath,
		Tags:        opts.Tags,
		Prefix:      opts.PrefixOutputFile,
		HeaderHash:  headerHash(opts.Header),
		Files:       metaFiles,
		ContentHash: contentHash,
		RootHash:    rootHash,
	}
	writeCacheMeta(metaKey, meta)
	return contentHash, nil
}

// packageFiles returns Go files for the local module packages in a dependency graph.
// External packages (under GOMODCACHE or GOROOT) are excluded — their integrity
// is tracked via go.mod/go.sum in extraCacheFiles.
func packageFiles(root *packages.Package) []string {
	excludePrefixes := externalPrefixes()
	seen := make(map[string]struct{})
	var files []string
	stack := []*packages.Package{root}
	for len(stack) > 0 {
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if p == nil {
			continue
		}
		if _, ok := seen[p.PkgPath]; ok {
			continue
		}
		seen[p.PkgPath] = struct{}{}
		goFiles := p.CompiledGoFiles
		if len(goFiles) == 0 {
			goFiles = p.GoFiles
		}
		if len(goFiles) > 0 && !isExternalPath(goFiles[0], excludePrefixes) {
			files = append(files, goFiles...)
		}
		for _, imp := range p.Imports {
			stack = append(stack, imp)
		}
	}
	return files
}

// cachedExternalPrefixes caches the result of externalPrefixes so it is
// computed at most once per process.
var cachedExternalPrefixes struct {
	once     sync.Once
	prefixes []string
}

// externalPrefixes returns path prefixes for directories containing external
// (non-local) Go packages: GOMODCACHE and GOROOT. Cached after first call.
func externalPrefixes() []string {
	cachedExternalPrefixes.once.Do(func() {
		var prefixes []string
		if modCache := os.Getenv("GOMODCACHE"); modCache != "" {
			prefixes = append(prefixes, filepath.Clean(modCache)+string(filepath.Separator))
		}
		if gopath := os.Getenv("GOPATH"); gopath != "" {
			prefixes = append(prefixes, filepath.Join(filepath.Clean(gopath), "pkg", "mod")+string(filepath.Separator))
		}
		if home, err := os.UserHomeDir(); err == nil {
			prefixes = append(prefixes, filepath.Join(home, "go", "pkg", "mod")+string(filepath.Separator))
		}
		if goroot := runtime.GOROOT(); goroot != "" {
			prefixes = append(prefixes, filepath.Clean(goroot)+string(filepath.Separator))
		}
		cachedExternalPrefixes.prefixes = prefixes
	})
	return cachedExternalPrefixes.prefixes
}

// isExternalPath reports whether the file path is under an external prefix.
func isExternalPath(path string, prefixes []string) bool {
	clean := filepath.Clean(path)
	for _, p := range prefixes {
		if strings.HasPrefix(clean, p) {
			return true
		}
	}
	return false
}

// cacheMetaKey builds the key for a package's cache metadata entry.
func cacheMetaKey(pkg *packages.Package, opts *GenerateOptions) string {
	h, release := getPooledSHA256()
	defer release()
	io.WriteString(h, cacheVersion)
	h.Write([]byte{0})
	io.WriteString(h, pkg.PkgPath)
	h.Write([]byte{0})
	io.WriteString(h, opts.Tags)
	h.Write([]byte{0})
	io.WriteString(h, opts.PrefixOutputFile)
	h.Write([]byte{0})
	io.WriteString(h, headerHash(opts.Header))
	return sumHex(h)
}

// cacheMetaPath returns the on-disk path for a cache metadata key.
func cacheMetaPath(key string) string {
	return filepath.Join(cacheDir(), key+".json")
}

// readCacheMeta loads a cached metadata entry if it exists.
func readCacheMeta(key string) (*cacheMeta, bool) {
	data, err := osReadFile(cacheMetaPath(key))
	if err != nil {
		return nil, false
	}
	var meta cacheMeta
	if err := jsonUnmarshal(data, &meta); err != nil {
		return nil, false
	}
	return &meta, true
}

// writeCacheMeta persists cache metadata to disk.
func writeCacheMeta(key string, meta *cacheMeta) {
	dir := cacheDir()
	if err := osMkdirAll(dir, 0755); err != nil {
		return
	}
	data, err := jsonMarshal(meta)
	if err != nil {
		return
	}
	tmp, err := osCreateTemp(dir, key+".meta-")
	if err != nil {
		return
	}
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		osRemove(tmp.Name())
		return
	}
	path := cacheMetaPath(key)
	if err := osRename(tmp.Name(), path); err != nil {
		osRemove(tmp.Name())
	}
}

// cacheMetaMatches reports whether metadata matches the current package inputs.
func cacheMetaMatches(meta *cacheMeta, pkg *packages.Package, opts *GenerateOptions, files []string) bool {
	if meta.Version != cacheVersion {
		return false
	}
	if meta.PkgPath != pkg.PkgPath || meta.Tags != opts.Tags || meta.Prefix != opts.PrefixOutputFile {
		return false
	}
	if meta.HeaderHash != headerHash(opts.Header) {
		return false
	}
	if len(meta.Files) != len(files) {
		return false
	}
	var statCache *FileStatCache
	if opts != nil {
		statCache = opts.FileStatCache
	}
	current, err := buildCacheFiles(files, statCache)
	if err != nil {
		return false
	}
	for i := range meta.Files {
		if meta.Files[i] != current[i] {
			return false
		}
	}
	rootFiles := rootPackageFiles(pkg)
	if len(rootFiles) == 0 || meta.RootHash == "" {
		return false
	}
	sort.Strings(rootFiles)
	var cache *FileHashCache
	if opts != nil {
		cache = opts.FileHashCache
	}
	rootHash, err := hashFiles(rootFiles, cache)
	if err != nil || rootHash != meta.RootHash {
		return false
	}
	return meta.ContentHash != ""
}

// buildCacheFiles converts file paths into cache metadata entries.
// If cache is non-nil, stat results are reused to avoid redundant os.Stat.
func buildCacheFiles(files []string, cache *FileStatCache) ([]cacheFile, error) {
	out := make([]cacheFile, len(files))
	for i, name := range files {
		path := filepath.Clean(name)
		if cache != nil {
			if cf, ok := cache.load(path); ok {
				out[i] = *cf
				continue
			}
		}
		info, err := osStat(name)
		if err != nil {
			return nil, err
		}
		cf := cacheFile{
			Path:    path,
			Size:    info.Size(),
			ModTime: info.ModTime().UnixNano(),
		}
		if cache != nil {
			cache.store(path, &cf)
		}
		out[i] = cf
	}
	return out, nil
}

// headerHash returns a stable hash of the generated header content.
func headerHash(header []byte) string {
	if len(header) == 0 {
		return ""
	}
	sum := sha256.Sum256(header)
	return hex.EncodeToString(sum[:])
}

// contentHashForFiles hashes the current package inputs using file paths.
func contentHashForFiles(pkg *packages.Package, opts *GenerateOptions, files []string) (string, error) {
	return contentHashForPaths(pkg.PkgPath, opts, files)
}

// fileContentHash returns the SHA256 hex hash of the file's content, using cache if set.
func fileContentHash(path string, cache *FileHashCache) (string, error) {
	if cache != nil {
		if h, ok := cache.load(path); ok {
			return h, nil
		}
	}
	data, err := osReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	h := hex.EncodeToString(sum[:])
	if cache != nil {
		cache.store(path, h)
	}
	return h, nil
}

// fileContentHashesParallel returns SHA256 hex hashes for each path in order, reading
// cache misses in parallel to reduce I/O wall time.
func fileContentHashesParallel(paths []string, cache *FileHashCache) ([]string, error) {
	hashes := make([]string, len(paths))
	var toRead []int
	for i, path := range paths {
		if cache != nil {
			if h, ok := cache.load(path); ok {
				hashes[i] = h
				continue
			}
		}
		toRead = append(toRead, i)
	}
	if len(toRead) == 0 {
		return hashes, nil
	}
	type readResult struct {
		data []byte
		err  error
	}
	results := make([]readResult, len(toRead))
	workers := maxParallelReads
	if n := runtime.GOMAXPROCS(0); n < workers && n > 0 {
		workers = n
	}
	if workers > len(toRead) {
		workers = len(toRead)
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	for j, idx := range toRead {
		wg.Add(1)
		go func(j int, path string) {
			defer wg.Done()
			sem <- struct{}{}
			data, err := osReadFile(path)
			<-sem
			results[j] = readResult{data, err}
		}(j, paths[idx])
	}
	wg.Wait()
	for j, idx := range toRead {
		if results[j].err != nil {
			return nil, results[j].err
		}
		sum := sha256.Sum256(results[j].data)
		h := hex.EncodeToString(sum[:])
		if cache != nil {
			cache.store(paths[idx], h)
		}
		hashes[idx] = h
	}
	return hashes, nil
}

// contentHashForPaths hashes the provided file contents and options.
func contentHashForPaths(pkgPath string, opts *GenerateOptions, files []string) (string, error) {
	h, release := getPooledSHA256()
	defer release()
	io.WriteString(h, cacheVersion)
	h.Write([]byte{0})
	io.WriteString(h, pkgPath)
	h.Write([]byte{0})
	io.WriteString(h, opts.Tags)
	h.Write([]byte{0})
	io.WriteString(h, opts.PrefixOutputFile)
	h.Write([]byte{0})
	io.WriteString(h, headerHash(opts.Header))
	h.Write([]byte{0})
	var cache *FileHashCache
	if opts != nil {
		cache = opts.FileHashCache
	}
	fileHashes, err := fileContentHashesParallel(files, cache)
	if err != nil {
		return "", err
	}
	for i, name := range files {
		io.WriteString(h, name)
		h.Write([]byte{0})
		io.WriteString(h, fileHashes[i])
		h.Write([]byte{0})
	}
	return sumHex(h), nil
}

// rootPackageFiles returns the direct Go files for the root package.
func rootPackageFiles(pkg *packages.Package) []string {
	if pkg == nil {
		return nil
	}
	if len(pkg.CompiledGoFiles) > 0 {
		return append([]string(nil), pkg.CompiledGoFiles...)
	}
	if len(pkg.GoFiles) > 0 {
		return append([]string(nil), pkg.GoFiles...)
	}
	return nil
}

// hashFiles returns a combined content hash for the provided paths.
// If cache is non-nil, file content hashes are reused to avoid redundant reads.
func hashFiles(files []string, cache *FileHashCache) (string, error) {
	if len(files) == 0 {
		return "", nil
	}
	fileHashes, err := fileContentHashesParallel(files, cache)
	if err != nil {
		return "", err
	}
	h, release := getPooledSHA256()
	defer release()
	for i, name := range files {
		io.WriteString(h, name)
		h.Write([]byte{0})
		io.WriteString(h, fileHashes[i])
		h.Write([]byte{0})
	}
	return sumHex(h), nil
}
