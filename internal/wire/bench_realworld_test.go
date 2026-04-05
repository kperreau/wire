// Copyright 2026 The Wire Authors
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
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// homiclipBackendPath is the path to the real-world project used for benchmarks.
// Set via WIRE_BENCH_PROJECT env var, defaults to ~/cursor/homiclip-backend.
func homiclipBackendPath(b *testing.B) string {
	b.Helper()
	if p := os.Getenv("WIRE_BENCH_PROJECT"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		b.Skipf("cannot find home dir: %v", err)
	}
	path := home + "/cursor/homiclip-backend"
	if _, err := os.Stat(path + "/go.mod"); err != nil {
		b.Skipf("homiclip-backend not found at %s: %v", path, err)
	}
	return path
}

func benchEnv() []string {
	return append(os.Environ(), "GOWORK=off")
}

// BenchmarkRealWorldColdNoCache benchmarks full Generate on homiclip-backend
// with an empty cache (simulates post-genall scenario).
func BenchmarkRealWorldColdNoCache(b *testing.B) {
	root := homiclipBackendPath(b)
	env := benchEnv()
	ctx := context.Background()

	for b.Loop() {
		b.StopTimer()
		tmpDir := b.TempDir()
		origTmp := os.Getenv("TMPDIR")
		os.Setenv("TMPDIR", tmpDir)
		// Reset the cached external prefixes so they are recomputed
		cachedExternalPrefixes.once = sync.Once{}
		cachedExternalPrefixes.prefixes = nil
		b.StartTimer()

		opts := &GenerateOptions{}
		_, errs := Generate(ctx, root, env, []string{"./..."}, opts)

		b.StopTimer()
		os.Setenv("TMPDIR", origTmp)
		cachedExternalPrefixes.once = sync.Once{}
		cachedExternalPrefixes.prefixes = nil
		b.StartTimer()

		if len(errs) > 0 {
			b.Fatalf("Generate errors: %v", errs)
		}
	}
}

// BenchmarkRealWorldWarmCache benchmarks Generate on homiclip-backend
// with manifest cache hit (no files changed).
func BenchmarkRealWorldWarmCache(b *testing.B) {
	root := homiclipBackendPath(b)
	env := benchEnv()
	ctx := context.Background()

	tmpDir := b.TempDir()
	origTmp := os.Getenv("TMPDIR")
	os.Setenv("TMPDIR", tmpDir)
	defer os.Setenv("TMPDIR", origTmp)

	// Warm cache
	opts := &GenerateOptions{}
	if _, errs := Generate(ctx, root, env, []string{"./..."}, opts); len(errs) > 0 {
		b.Fatalf("warm Generate: %v", errs)
	}

	for b.Loop() {
		opts := &GenerateOptions{}
		_, errs := Generate(ctx, root, env, []string{"./..."}, opts)
		if len(errs) > 0 {
			b.Fatalf("Generate errors: %v", errs)
		}
	}
}

// BenchmarkRealWorldColdWithTiming benchmarks cold Generate with detailed timing
// breakdown to identify bottlenecks.
func BenchmarkRealWorldColdWithTiming(b *testing.B) {
	root := homiclipBackendPath(b)
	env := benchEnv()

	// Collect timing data over all iterations
	type timingEntry struct {
		label    string
		duration time.Duration
	}
	var mu sync.Mutex
	var allTimings []timingEntry

	ctx := WithTiming(context.Background(), func(label string, d time.Duration) {
		mu.Lock()
		allTimings = append(allTimings, timingEntry{label, d})
		mu.Unlock()
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		tmpDir := b.TempDir()
		origTmp := os.Getenv("TMPDIR")
		os.Setenv("TMPDIR", tmpDir)
		cachedExternalPrefixes.once = sync.Once{}
		cachedExternalPrefixes.prefixes = nil
		b.StartTimer()

		opts := &GenerateOptions{}
		_, errs := Generate(ctx, root, env, []string{"./..."}, opts)

		b.StopTimer()
		os.Setenv("TMPDIR", origTmp)
		cachedExternalPrefixes.once = sync.Once{}
		cachedExternalPrefixes.prefixes = nil
		b.StartTimer()

		if len(errs) > 0 {
			b.Fatalf("Generate errors: %v", errs)
		}
	}

	// Report aggregated timing
	if b.N > 0 {
		b.StopTimer()
		agg := make(map[string]time.Duration)
		counts := make(map[string]int)
		for _, t := range allTimings {
			agg[t.label] += t.duration
			counts[t.label]++
		}
		for label, total := range agg {
			avg := total / time.Duration(counts[label])
			b.ReportMetric(float64(avg.Microseconds()), label+"-avg-µs")
		}
	}
}

// BenchmarkRealWorldLoad benchmarks just the package loading phase.
func BenchmarkRealWorldLoad(b *testing.B) {
	root := homiclipBackendPath(b)
	env := benchEnv()
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pkgs, _, errs := load(ctx, root, env, "", []string{"./..."})
		if len(errs) > 0 {
			b.Fatalf("load errors: %v", errs)
		}
		if len(pkgs) == 0 {
			b.Fatal("no packages loaded")
		}
	}
}

// BenchmarkRealWorldPackagesWithWire benchmarks the wire package filtering phase.
func BenchmarkRealWorldPackagesWithWire(b *testing.B) {
	root := homiclipBackendPath(b)
	env := benchEnv()
	ctx := context.Background()

	pkgs, _, errs := load(ctx, root, env, "", []string{"./..."})
	if len(errs) > 0 {
		b.Fatalf("load: %v", errs)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		wirePkgs := packagesWithWire(pkgs)
		if len(wirePkgs) == 0 {
			b.Fatal("no wire packages found")
		}
	}
	b.ReportMetric(float64(len(pkgs)), "total-pkgs")
}

// BenchmarkRealWorldCacheKey benchmarks cache key computation for all wire packages.
func BenchmarkRealWorldCacheKey(b *testing.B) {
	root := homiclipBackendPath(b)
	env := benchEnv()
	ctx := context.Background()

	pkgs, _, errs := load(ctx, root, env, "", []string{"./..."})
	if len(errs) > 0 {
		b.Fatalf("load: %v", errs)
	}
	wirePkgs := packagesWithWire(pkgs)

	b.Run("cold", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			tmpDir := b.TempDir()
			origTmp := os.Getenv("TMPDIR")
			os.Setenv("TMPDIR", tmpDir)
			b.StartTimer()

			for _, pkg := range wirePkgs {
				opts := &GenerateOptions{
					FileHashCache: &FileHashCache{},
					FileStatCache: &FileStatCache{},
				}
				_, err := cacheKeyForPackage(pkg, opts)
				if err != nil {
					b.Fatalf("cacheKeyForPackage: %v", err)
				}
			}

			b.StopTimer()
			os.Setenv("TMPDIR", origTmp)
			b.StartTimer()
		}
	})

	b.Run("warm_caches", func(b *testing.B) {
		hashCache := &FileHashCache{}
		statCache := &FileStatCache{}
		// Warm file caches
		for _, pkg := range wirePkgs {
			opts := &GenerateOptions{
				FileHashCache: hashCache,
				FileStatCache: statCache,
			}
			cacheKeyForPackage(pkg, opts)
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			for _, pkg := range wirePkgs {
				b.StopTimer()
				tmpDir := b.TempDir()
				origTmp := os.Getenv("TMPDIR")
				os.Setenv("TMPDIR", tmpDir)
				b.StartTimer()

				opts := &GenerateOptions{
					FileHashCache: hashCache,
					FileStatCache: statCache,
				}
				_, err := cacheKeyForPackage(pkg, opts)
				if err != nil {
					b.Fatalf("cacheKeyForPackage: %v", err)
				}

				b.StopTimer()
				os.Setenv("TMPDIR", origTmp)
				b.StartTimer()
			}
		}
	})
}

// BenchmarkRealWorldManifestValid benchmarks manifest validation for homiclip-backend.
func BenchmarkRealWorldManifestValid(b *testing.B) {
	root := homiclipBackendPath(b)
	env := benchEnv()
	ctx := context.Background()

	tmpDir := b.TempDir()
	origTmp := os.Getenv("TMPDIR")
	os.Setenv("TMPDIR", tmpDir)
	defer os.Setenv("TMPDIR", origTmp)

	opts := &GenerateOptions{}
	results, errs := Generate(ctx, root, env, []string{"./..."}, opts)
	if len(errs) > 0 {
		b.Fatalf("Generate: %v", errs)
	}
	hasContent := false
	for _, r := range results {
		if len(r.Content) > 0 {
			hasContent = true
		}
	}
	if !hasContent {
		b.Fatal("Generate produced no content")
	}

	key := manifestKey(root, env, []string{"./..."}, opts)
	manifest, ok := readManifest(key)
	if !ok {
		b.Fatal("no manifest found after Generate")
	}

	// Report manifest stats
	totalFiles := 0
	for _, pkg := range manifest.Packages {
		totalFiles += len(pkg.Files)
	}
	b.ReportMetric(float64(len(manifest.Packages)), "manifest-pkgs")
	b.ReportMetric(float64(totalFiles), "manifest-files")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !manifestValid(manifest, &GenerateOptions{
			FileHashCache: &FileHashCache{},
			FileStatCache: &FileStatCache{},
		}) {
			b.Fatal("manifest should be valid")
		}
	}
}

// BenchmarkRealWorldPackageFiles benchmarks transitive file collection for wire packages.
func BenchmarkRealWorldPackageFiles(b *testing.B) {
	root := homiclipBackendPath(b)
	env := benchEnv()
	ctx := context.Background()

	pkgs, _, errs := load(ctx, root, env, "", []string{"./..."})
	if len(errs) > 0 {
		b.Fatalf("load: %v", errs)
	}
	wirePkgs := packagesWithWire(pkgs)

	b.ResetTimer()
	totalFiles := 0
	for i := 0; i < b.N; i++ {
		totalFiles = 0
		for _, pkg := range wirePkgs {
			files := packageFiles(pkg)
			totalFiles += len(files)
		}
	}
	b.ReportMetric(float64(totalFiles), "total-files")
	b.ReportMetric(float64(len(wirePkgs)), "wire-pkgs")
}

// BenchmarkRealWorldHashFiles benchmarks file hashing for all transitive deps of wire packages.
func BenchmarkRealWorldHashFiles(b *testing.B) {
	root := homiclipBackendPath(b)
	env := benchEnv()
	ctx := context.Background()

	pkgs, _, errs := load(ctx, root, env, "", []string{"./..."})
	if len(errs) > 0 {
		b.Fatalf("load: %v", errs)
	}
	wirePkgs := packagesWithWire(pkgs)

	// Collect all unique files from all wire packages
	allFiles := make(map[string]struct{})
	for _, pkg := range wirePkgs {
		for _, f := range packageFiles(pkg) {
			allFiles[f] = struct{}{}
		}
	}
	files := make([]string, 0, len(allFiles))
	for f := range allFiles {
		files = append(files, f)
	}

	b.ReportMetric(float64(len(files)), "unique-files")

	b.Run("no_cache", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, err := hashFiles(files, nil)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("with_cache", func(b *testing.B) {
		cache := &FileHashCache{}
		hashFiles(files, cache)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, err := hashFiles(files, cache)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkRealWorldBuildCacheFiles benchmarks stat operations for all transitive deps.
func BenchmarkRealWorldBuildCacheFiles(b *testing.B) {
	root := homiclipBackendPath(b)
	env := benchEnv()
	ctx := context.Background()

	pkgs, _, errs := load(ctx, root, env, "", []string{"./..."})
	if len(errs) > 0 {
		b.Fatalf("load: %v", errs)
	}
	wirePkgs := packagesWithWire(pkgs)

	allFiles := make(map[string]struct{})
	for _, pkg := range wirePkgs {
		for _, f := range packageFiles(pkg) {
			allFiles[f] = struct{}{}
		}
	}
	files := make([]string, 0, len(allFiles))
	for f := range allFiles {
		files = append(files, f)
	}

	b.ReportMetric(float64(len(files)), "unique-files")

	b.Run("no_cache", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, err := buildCacheFiles(files, nil)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("with_cache", func(b *testing.B) {
		cache := &FileStatCache{}
		buildCacheFiles(files, cache)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, err := buildCacheFiles(files, cache)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

// TestRealWorldTimingBreakdown is not a benchmark — it runs Generate once
// and prints a detailed timing breakdown. Run with: go test -run TestRealWorldTimingBreakdown -v
func TestRealWorldTimingBreakdown(t *testing.T) {
	root := os.Getenv("WIRE_BENCH_PROJECT")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip("cannot find home dir")
		}
		root = home + "/cursor/homiclip-backend"
	}
	if _, err := os.Stat(root + "/go.mod"); err != nil {
		t.Skipf("project not found at %s", root)
	}

	env := benchEnv()

	type entry struct {
		label    string
		duration time.Duration
	}
	var mu sync.Mutex
	var timings []entry

	ctx := WithTiming(context.Background(), func(label string, d time.Duration) {
		mu.Lock()
		timings = append(timings, entry{label, d})
		mu.Unlock()
	})

	// Cold run
	tmpDir := t.TempDir()
	origTmp := os.Getenv("TMPDIR")
	os.Setenv("TMPDIR", tmpDir)
	defer os.Setenv("TMPDIR", origTmp)
	cachedExternalPrefixes.once = sync.Once{}
	cachedExternalPrefixes.prefixes = nil

	coldStart := time.Now()
	opts := &GenerateOptions{}
	results, errs := Generate(ctx, root, env, []string{"./..."}, opts)
	coldTotal := time.Since(coldStart)

	if len(errs) > 0 {
		t.Fatalf("Generate errors: %v", errs)
	}

	t.Logf("=== COLD RUN (no cache) ===")
	t.Logf("Total: %v", coldTotal)
	t.Logf("Packages generated: %d", len(results))
	contentCount := 0
	for _, r := range results {
		if len(r.Content) > 0 {
			contentCount++
		}
	}
	t.Logf("Packages with content: %d", contentCount)
	t.Logf("")
	t.Logf("--- Timing breakdown ---")
	for _, e := range timings {
		t.Logf("  %-60s %v", e.label, e.duration)
	}

	// Warm run
	timings = nil
	warmStart := time.Now()
	opts2 := &GenerateOptions{}
	_, errs2 := Generate(ctx, root, env, []string{"./..."}, opts2)
	warmTotal := time.Since(warmStart)

	if len(errs2) > 0 {
		t.Fatalf("Generate errors (warm): %v", errs2)
	}

	t.Logf("")
	t.Logf("=== WARM RUN (with cache) ===")
	t.Logf("Total: %v", warmTotal)
	t.Logf("Speedup: %.1fx", float64(coldTotal)/float64(warmTotal))
	if len(timings) > 0 {
		t.Logf("--- Timing breakdown ---")
		for _, e := range timings {
			t.Logf("  %-60s %v", e.label, e.duration)
		}
	}

	// Simulated cache invalidation: touch a file and re-run
	t.Logf("")
	t.Logf("=== INVALIDATED CACHE RUN (simulate genall) ===")
	timings = nil

	// Touch go.mod to simulate genall invalidation
	gomod := root + "/go.mod"
	info, err := os.Stat(gomod)
	if err != nil {
		t.Fatalf("stat go.mod: %v", err)
	}
	now := time.Now()
	os.Chtimes(gomod, now, now)
	defer os.Chtimes(gomod, info.ModTime(), info.ModTime())

	invalidStart := time.Now()
	opts3 := &GenerateOptions{}
	_, errs3 := Generate(ctx, root, env, []string{"./..."}, opts3)
	invalidTotal := time.Since(invalidStart)

	if len(errs3) > 0 {
		t.Fatalf("Generate errors (invalidated): %v", errs3)
	}

	t.Logf("Total: %v", invalidTotal)
	if len(timings) > 0 {
		t.Logf("--- Timing breakdown ---")
		for _, e := range timings {
			t.Logf("  %-60s %v", e.label, e.duration)
		}
	}

	// Summary
	t.Logf("")
	t.Logf("=== SUMMARY ===")
	t.Logf("Cold (no cache):       %v", coldTotal)
	t.Logf("Warm (cache hit):      %v", warmTotal)
	t.Logf("Invalidated (genall):  %v", invalidTotal)
	t.Logf("Cold/Warm ratio:       %.1fx", float64(coldTotal)/float64(warmTotal))
	t.Logf("Invalidated/Warm:      %.1fx", float64(invalidTotal)/float64(warmTotal))
}

// BenchmarkRealWorldEnvHash benchmarks environment hash computation
// (env can be large — hundreds of vars).
func BenchmarkRealWorldEnvHash(b *testing.B) {
	env := benchEnv()
	b.ReportMetric(float64(len(env)), "env-vars")

	for b.Loop() {
		h := envHash(env)
		if h == "" {
			b.Fatal("empty hash")
		}
	}
}

// BenchmarkRealWorldManifestKey benchmarks manifest key computation.
func BenchmarkRealWorldManifestKey(b *testing.B) {
	root := homiclipBackendPath(b)
	env := benchEnv()
	opts := &GenerateOptions{}

	for b.Loop() {
		k := manifestKey(root, env, []string{"./..."}, opts)
		if k == "" {
			b.Fatal("empty key")
		}
	}
}

// BenchmarkRealWorldManifestReadWrite benchmarks manifest serialization.
func BenchmarkRealWorldManifestReadWrite(b *testing.B) {
	root := homiclipBackendPath(b)
	env := benchEnv()
	ctx := context.Background()

	tmpDir := b.TempDir()
	origTmp := os.Getenv("TMPDIR")
	os.Setenv("TMPDIR", tmpDir)
	defer os.Setenv("TMPDIR", origTmp)

	// Generate to create manifest
	opts := &GenerateOptions{}
	if _, errs := Generate(ctx, root, env, []string{"./..."}, opts); len(errs) > 0 {
		b.Fatalf("Generate: %v", errs)
	}

	key := manifestKey(root, env, []string{"./..."}, opts)
	manifest, ok := readManifest(key)
	if !ok {
		b.Fatal("no manifest")
	}

	// Measure the JSON size
	data, _ := jsonMarshal(manifest)
	b.ReportMetric(float64(len(data)), "manifest-bytes")

	b.Run("marshal", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, err := jsonMarshal(manifest)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("unmarshal", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var m cacheManifest
			if err := jsonUnmarshal(data, &m); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("read_from_disk", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, ok := readManifest(key)
			if !ok {
				b.Fatal("manifest not found")
			}
		}
	})
}

// BenchmarkRealWorldExtraCacheFiles benchmarks extra cache file collection (go.mod etc).
func BenchmarkRealWorldExtraCacheFiles(b *testing.B) {
	root := homiclipBackendPath(b)

	var count int
	for b.Loop() {
		files := extraCacheFiles(root)
		count = len(files)
	}
	b.ReportMetric(float64(count), "extra-files")
}

func init() {
	// Print a reminder about how to run these benchmarks
	if os.Getenv("WIRE_BENCH_HELP") == "1" {
		fmt.Fprintf(os.Stderr, `
Real-world benchmarks for Wire against homiclip-backend.

Run all benchmarks:
  go test -bench=BenchmarkRealWorld -benchtime=3x -timeout=10m ./internal/wire/

Run timing breakdown:
  go test -run=TestRealWorldTimingBreakdown -v -timeout=10m ./internal/wire/

Run specific benchmark:
  go test -bench=BenchmarkRealWorldColdNoCache -benchtime=1x -timeout=10m ./internal/wire/

Override project path:
  WIRE_BENCH_PROJECT=/path/to/project go test -bench=BenchmarkRealWorld ...

CPU profile:
  go test -bench=BenchmarkRealWorldColdNoCache -benchtime=3x -cpuprofile=cold.prof -timeout=10m ./internal/wire/
  go tool pprof cold.prof
`)
	}
}
