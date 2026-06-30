package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Dataset is a labeled set of cases plus the artifact root they read from.
type Dataset struct {
	// ArtifactRoot is the directory the local backend reads build artifacts
	// from. Defaults to <dataset dir>/artifacts.
	ArtifactRoot string `json:"-"`
	Cases        []Case `json:"cases"`
}

// LoadDataset reads <dir>/cases.json and resolves the artifact root to
// <dir>/artifacts unless the file overrides it.
func LoadDataset(dir string) (*Dataset, error) {
	data, err := os.ReadFile(filepath.Join(dir, "cases.json"))
	if err != nil {
		return nil, fmt.Errorf("eval: reading cases.json: %w", err)
	}
	var raw struct {
		ArtifactRoot string `json:"artifact_root"`
		Cases        []Case `json:"cases"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("eval: parsing cases.json: %w", err)
	}
	if len(raw.Cases) == 0 {
		return nil, fmt.Errorf("eval: dataset %s has no cases", dir)
	}
	root := raw.ArtifactRoot
	if root == "" {
		root = filepath.Join(dir, "artifacts")
	} else if !filepath.IsAbs(root) {
		root = filepath.Join(dir, root)
	}
	seen := map[string]bool{}
	for _, c := range raw.Cases {
		if c.Name == "" {
			return nil, fmt.Errorf("eval: a case is missing a name")
		}
		if seen[c.Name] {
			return nil, fmt.Errorf("eval: duplicate case name %q", c.Name)
		}
		seen[c.Name] = true
		if c.BuildPrefix == "" || c.TestName == "" {
			return nil, fmt.Errorf("eval: case %q needs build_prefix and test_name", c.Name)
		}
	}
	return &Dataset{ArtifactRoot: root, Cases: raw.Cases}, nil
}

// Fingerprint hashes the case identities, labels, and artifact contents so an
// A/B comparison can detect that two scorecards were computed on different
// datasets, labels, or evidence. It is order-independent over cases and files.
func (d *Dataset) Fingerprint() string {
	names := make([]string, len(d.Cases))
	byName := make(map[string]Case, len(d.Cases))
	for i, c := range d.Cases {
		names[i] = c.Name
		byName[c.Name] = c
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		c := byName[n]
		labels, _ := json.Marshal(c.Labels)
		fmt.Fprintf(h, "case|%s|%s|%s|%s\n", c.Name, c.BuildPrefix, c.TestName, labels)
	}
	hashArtifacts(h, d.ArtifactRoot)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// hashArtifacts folds every artifact file (relative path, size, content hash)
// into h so the fingerprint changes when the recorded evidence changes, even if
// case labels do not. A missing root contributes nothing (LoadDataset/the runner
// surface that separately).
func hashArtifacts(h io.Writer, root string) {
	if root == "" {
		return
	}
	type entry struct {
		rel  string
		size int64
		sum  string
	}
	var entries []entry
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		data, derr := os.ReadFile(p)
		if derr != nil {
			return nil
		}
		sum := sha256.Sum256(data)
		entries = append(entries, entry{rel: filepath.ToSlash(rel), size: info.Size(), sum: hex.EncodeToString(sum[:])})
		return nil
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	for _, e := range entries {
		fmt.Fprintf(h, "file|%s|%d|%s\n", e.rel, e.size, e.sum)
	}
}
