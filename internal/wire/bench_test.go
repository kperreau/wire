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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// setupBenchProject creates a temporary Go project with N dependency packages,
// each having M files.
func setupBenchProject(b *testing.B, numDeps, filesPerDep int) string {
	b.Helper()
	repoRoot := mustRepoRootB(b)
	root := b.TempDir()

	gomod := fmt.Sprintf(`module example.com/bench

go 1.19

require github.com/goforj/wire v0.0.0
replace github.com/goforj/wire => %s
`, repoRoot)
	writeFileB(b, filepath.Join(root, "go.mod"), gomod)

	// Build a chain: dep0 provides string, dep1..N take and return different types
	var imports []string
	var providers []string

	// First dep provides a base string
	imports = append(imports, "\t\"example.com/bench/dep0\"")
	providers = append(providers, "dep0.ProvideDep0")
	writeFileB(b, filepath.Join(root, "dep0", "dep0.go"), `package dep0

type Dep0Result struct {
	Value string
}

func ProvideDep0() Dep0Result {
	return Dep0Result{Value: "hello"}
}
`)
	for j := 1; j < filesPerDep; j++ {
		writeFileB(b, filepath.Join(root, "dep0", fmt.Sprintf("helper%d.go", j)), fmt.Sprintf(`package dep0

func helper%d() string { return "h%d" }
`, j, j))
	}

	for i := 1; i < numDeps; i++ {
		pkgName := fmt.Sprintf("dep%d", i)
		prevPkg := fmt.Sprintf("dep%d", i-1)
		titleName := strings.ToUpper(pkgName[:1]) + pkgName[1:]
		prevTitle := strings.ToUpper(prevPkg[:1]) + prevPkg[1:]
		imports = append(imports, fmt.Sprintf("\t\"example.com/bench/%s\"", pkgName))
		providers = append(providers, fmt.Sprintf("%s.Provide%s", pkgName, titleName))

		writeFileB(b, filepath.Join(root, pkgName, fmt.Sprintf("%s.go", pkgName)), fmt.Sprintf(`package %s

import "example.com/bench/%s"

type %sResult struct {
	Prev %s.%sResult
}

func Provide%s(prev %s.%sResult) %sResult {
	return %sResult{Prev: prev}
}
`, pkgName, prevPkg, titleName, prevPkg, prevTitle, titleName, prevPkg, prevTitle, titleName, titleName))

		for j := 1; j < filesPerDep; j++ {
			writeFileB(b, filepath.Join(root, pkgName, fmt.Sprintf("helper%d.go", j)), fmt.Sprintf(`package %s

func helper%d() string { return "h%d" }
`, pkgName, j, j))
		}
	}

	// The injector returns the last type in the chain
	lastPkg := fmt.Sprintf("dep%d", numDeps-1)
	lastTitle := strings.ToUpper(lastPkg[:1]) + lastPkg[1:]
	wireFile := fmt.Sprintf(`//go:build wireinject
// +build wireinject

package app

import (
%s
	"github.com/goforj/wire"
)

func Init() %s.%sResult {
	wire.Build(%s)
	return %s.%sResult{}
}
`, strings.Join(imports, "\n"), lastPkg, lastTitle, strings.Join(providers, ", "), lastPkg, lastTitle)
	writeFileB(b, filepath.Join(root, "app", "wire.go"), wireFile)

	cmd := exec.Command("go", "mod", "tidy")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		b.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	return root
}

func mustRepoRootB(b *testing.B) string {
	b.Helper()
	wd, err := os.Getwd()
	if err != nil {
		b.Fatalf("Getwd: %v", err)
	}
	repoRoot := filepath.Clean(filepath.Join(wd, "..", ".."))
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err != nil {
		b.Fatalf("repo root not found at %s: %v", repoRoot, err)
	}
	return repoRoot
}

func writeFileB(b *testing.B, path string, content string) {
	b.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		b.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		b.Fatalf("WriteFile: %v", err)
	}
}

// BenchmarkGenerateColdNoCache benchmarks full Generate with empty cache.
func BenchmarkGenerateColdNoCache(b *testing.B) {
	for _, numDeps := range []int{1, 5, 10} {
		b.Run(fmt.Sprintf("deps=%d", numDeps), func(b *testing.B) {
			root := setupBenchProject(b, numDeps, 3)
			env := append(os.Environ(), "GOWORK=off")
			ctx := context.Background()

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				tmpDir := b.TempDir()
				origTmp := os.Getenv("TMPDIR")
				os.Setenv("TMPDIR", tmpDir)
				b.StartTimer()

				opts := &GenerateOptions{}
				_, errs := Generate(ctx, root, env, []string{"./app"}, opts)

				b.StopTimer()
				os.Setenv("TMPDIR", origTmp)
				b.StartTimer()

				if len(errs) > 0 {
					b.Fatalf("Generate errors: %v", errs)
				}
			}
		})
	}
}

// BenchmarkGenerateWarmCache benchmarks Generate with manifest cache hit.
func BenchmarkGenerateWarmCache(b *testing.B) {
	for _, numDeps := range []int{1, 5, 10} {
		b.Run(fmt.Sprintf("deps=%d", numDeps), func(b *testing.B) {
			root := setupBenchProject(b, numDeps, 3)
			env := append(os.Environ(), "GOWORK=off")
			ctx := context.Background()

			tmpDir := b.TempDir()
			origTmp := os.Getenv("TMPDIR")
			os.Setenv("TMPDIR", tmpDir)
			defer os.Setenv("TMPDIR", origTmp)

			// Warm cache
			opts := &GenerateOptions{}
			if _, errs := Generate(ctx, root, env, []string{"./app"}, opts); len(errs) > 0 {
				b.Fatalf("warm Generate: %v", errs)
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				opts := &GenerateOptions{}
				_, errs := Generate(ctx, root, env, []string{"./app"}, opts)
				if len(errs) > 0 {
					b.Fatalf("Generate errors: %v", errs)
				}
			}
		})
	}
}

// BenchmarkCacheKeyForPackage benchmarks cache key computation.
func BenchmarkCacheKeyForPackage(b *testing.B) {
	for _, numDeps := range []int{1, 5, 10} {
		b.Run(fmt.Sprintf("deps=%d", numDeps), func(b *testing.B) {
			root := setupBenchProject(b, numDeps, 3)
			env := append(os.Environ(), "GOWORK=off")
			ctx := context.Background()

			tmpDir := b.TempDir()
			origTmp := os.Getenv("TMPDIR")
			os.Setenv("TMPDIR", tmpDir)
			defer os.Setenv("TMPDIR", origTmp)

			pkgs, _, errs := load(ctx, root, env, "", []string{"./app"})
			if len(errs) > 0 || len(pkgs) == 0 {
				b.Fatalf("load: %v", errs)
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				opts := &GenerateOptions{
					FileHashCache: &FileHashCache{},
					FileStatCache: &FileStatCache{},
				}
				_, err := cacheKeyForPackage(pkgs[0], opts)
				if err != nil {
					b.Fatalf("cacheKeyForPackage: %v", err)
				}
			}
		})
	}
}

// BenchmarkManifestValid benchmarks manifest validation speed.
func BenchmarkManifestValid(b *testing.B) {
	for _, numDeps := range []int{1, 5, 10} {
		b.Run(fmt.Sprintf("deps=%d", numDeps), func(b *testing.B) {
			root := setupBenchProject(b, numDeps, 3)
			env := append(os.Environ(), "GOWORK=off")
			ctx := context.Background()

			tmpDir := b.TempDir()
			origTmp := os.Getenv("TMPDIR")
			os.Setenv("TMPDIR", tmpDir)
			defer os.Setenv("TMPDIR", origTmp)

			// Generate creates fresh caches internally, so use opts that match
			opts := &GenerateOptions{}
			results, errs := Generate(ctx, root, env, []string{"./app"}, opts)
			if len(errs) > 0 {
				b.Fatalf("Generate: %v", errs)
			}

			// Verify generation produced content
			hasContent := false
			for _, r := range results {
				if len(r.Content) > 0 {
					hasContent = true
				}
				if len(r.Errs) > 0 {
					b.Fatalf("result errors: %v", r.Errs)
				}
			}
			if !hasContent {
				b.Fatal("Generate produced no content")
			}

			// Read manifest - Generate resets caches in opts, so use empty opts
			key := manifestKey(root, env, []string{"./app"}, opts)
			manifest, ok := readManifest(key)
			if !ok {
				b.Logf("manifest key: %s", key)
				b.Logf("cache dir: %s", cacheDir())
				entries, _ := os.ReadDir(cacheDir())
				for _, e := range entries {
					b.Logf("  cache file: %s", e.Name())
				}
				b.Fatal("no manifest")
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if !manifestValid(manifest, &GenerateOptions{
					FileHashCache: &FileHashCache{},
					FileStatCache: &FileStatCache{},
				}) {
					b.Fatal("manifest should be valid")
				}
			}
		})
	}
}

// BenchmarkPackageFiles benchmarks transitive file collection.
func BenchmarkPackageFiles(b *testing.B) {
	for _, numDeps := range []int{1, 5, 10} {
		b.Run(fmt.Sprintf("deps=%d", numDeps), func(b *testing.B) {
			root := setupBenchProject(b, numDeps, 3)
			env := append(os.Environ(), "GOWORK=off")
			ctx := context.Background()

			pkgs, _, errs := load(ctx, root, env, "", []string{"./app"})
			if len(errs) > 0 || len(pkgs) == 0 {
				b.Fatalf("load: %v", errs)
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				files := packageFiles(pkgs[0])
				if len(files) == 0 {
					b.Fatal("no files")
				}
			}
		})
	}
}

// BenchmarkHashFiles benchmarks file content hashing with and without cache.
func BenchmarkHashFiles(b *testing.B) {
	for _, numDeps := range []int{1, 5, 10} {
		b.Run(fmt.Sprintf("deps=%d", numDeps), func(b *testing.B) {
			root := setupBenchProject(b, numDeps, 3)
			env := append(os.Environ(), "GOWORK=off")
			ctx := context.Background()

			pkgs, _, errs := load(ctx, root, env, "", []string{"./app"})
			if len(errs) > 0 || len(pkgs) == 0 {
				b.Fatalf("load: %v", errs)
			}
			files := packageFiles(pkgs[0])

			b.Run("no_cache", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					if _, err := hashFiles(files, nil); err != nil {
						b.Fatal(err)
					}
				}
			})

			b.Run("with_cache", func(b *testing.B) {
				cache := &FileHashCache{}
				hashFiles(files, cache) // warm
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := hashFiles(files, cache); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

// BenchmarkBuildCacheFiles benchmarks stat-based file metadata collection.
func BenchmarkBuildCacheFiles(b *testing.B) {
	for _, numDeps := range []int{1, 5, 10} {
		b.Run(fmt.Sprintf("deps=%d", numDeps), func(b *testing.B) {
			root := setupBenchProject(b, numDeps, 3)
			env := append(os.Environ(), "GOWORK=off")
			ctx := context.Background()

			pkgs, _, errs := load(ctx, root, env, "", []string{"./app"})
			if len(errs) > 0 || len(pkgs) == 0 {
				b.Fatalf("load: %v", errs)
			}
			files := packageFiles(pkgs[0])

			b.Run("no_cache", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					if _, err := buildCacheFiles(files, nil); err != nil {
						b.Fatal(err)
					}
				}
			})

			b.Run("with_cache", func(b *testing.B) {
				cache := &FileStatCache{}
				buildCacheFiles(files, cache) // warm
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := buildCacheFiles(files, cache); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}
