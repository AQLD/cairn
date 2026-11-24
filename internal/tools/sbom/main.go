// Command sbom writes a CycloneDX 1.5 bill of materials for Go binaries,
// from the build information the Go toolchain embeds in each one: every
// module linked in, at the exact version and checksum it was built with, and
// the Go version. No dependencies, and nothing guessed from go.mod.
//
//	go run ./internal/tools/sbom -name cairn -version v0.3.0 bin/cairn bin/cairnd > cairn.cdx.json
package main

import (
	"debug/buildinfo"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type component struct {
	Type    string `json:"type"`
	BOMRef  string `json:"bom-ref"`
	Name    string `json:"name"`
	Version string `json:"version"`
	PURL    string `json:"purl"`
	Hashes  []hash `json:"hashes,omitempty"`
}

type hash struct {
	Alg     string `json:"alg"`
	Content string `json:"content"`
}

type dependency struct {
	Ref       string   `json:"ref"`
	DependsOn []string `json:"dependsOn"`
}

func main() {
	name := flag.String("name", "cairn", "product name")
	version := flag.String("version", "dev", "product version")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: sbom [-name N] [-version V] BINARY...")
		os.Exit(2)
	}
	mods := map[string]component{}
	goVersions := map[string]bool{}
	var binaries []component
	var deps []dependency
	for _, path := range flag.Args() {
		bi, err := buildinfo.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sbom: %s: %v\n", path, err)
			os.Exit(1)
		}
		goVersions[bi.GoVersion] = true
		bin := filepath.Base(path)
		ref := "bin:" + bin
		binaries = append(binaries, component{Type: "application", BOMRef: ref, Name: bin, Version: *version,
			PURL: "pkg:golang/" + bi.Main.Path + "@" + *version})
		d := dependency{Ref: ref}
		for _, m := range bi.Deps {
			if m.Replace != nil {
				m = m.Replace
			}
			c := component{Type: "library", BOMRef: "pkg:golang/" + m.Path + "@" + m.Version, Name: m.Path, Version: m.Version, PURL: "pkg:golang/" + m.Path + "@" + m.Version}
			if sum, ok := strings.CutPrefix(m.Sum, "h1:"); ok {
				if raw, err := base64.StdEncoding.DecodeString(sum); err == nil {
					c.Hashes = []hash{{Alg: "SHA-256", Content: hex.EncodeToString(raw)}}
				}
			}
			mods[c.BOMRef] = c
			d.DependsOn = append(d.DependsOn, c.BOMRef)
		}
		sort.Strings(d.DependsOn)
		deps = append(deps, d)
	}
	var gos []string
	for v := range goVersions {
		gos = append(gos, v)
	}
	sort.Strings(gos)
	for _, v := range gos {
		ref := "pkg:golang/stdlib@" + v
		mods[ref] = component{Type: "library", BOMRef: ref, Name: "stdlib", Version: v, PURL: ref}
		for i := range deps {
			deps[i].DependsOn = append(deps[i].DependsOn, ref)
		}
	}
	comps := append([]component{}, binaries...)
	var refs []string
	for r := range mods {
		refs = append(refs, r)
	}
	sort.Strings(refs)
	for _, r := range refs {
		comps = append(comps, mods[r])
	}
	doc := map[string]any{
		"bomFormat":   "CycloneDX",
		"specVersion": "1.5",
		"version":     1,
		"metadata": map[string]any{
			"timestamp": time.Now().UTC().Format(time.RFC3339),
			"component": map[string]any{"type": "application", "name": *name, "version": *version},
			"tools":     []map[string]string{{"name": "cairn internal/tools/sbom"}},
		},
		"components":   comps,
		"dependencies": deps,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		fmt.Fprintln(os.Stderr, "sbom:", err)
		os.Exit(1)
	}
}
