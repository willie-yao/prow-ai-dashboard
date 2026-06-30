package eval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDataset_Example(t *testing.T) {
	ds, err := LoadDataset(filepath.Join("..", "..", "eval", "dataset", "example"))
	if err != nil {
		t.Fatalf("LoadDataset: %v", err)
	}
	if len(ds.Cases) != 1 {
		t.Fatalf("cases = %d, want 1", len(ds.Cases))
	}
	c := ds.Cases[0]
	if c.Name != "control-plane-provisioning-timeout" {
		t.Errorf("name = %q", c.Name)
	}
	if c.Labels.IsTransient {
		t.Error("example case should be labeled a real bug")
	}
	if len(c.Labels.RootCauseKeywords) == 0 {
		t.Error("example case should have root-cause keywords")
	}
	// Artifact root resolves to <dir>/artifacts.
	if filepath.Base(ds.ArtifactRoot) != "artifacts" {
		t.Errorf("artifact root = %q", ds.ArtifactRoot)
	}
}

func TestLoadDataset_Errors(t *testing.T) {
	if _, err := LoadDataset(t.TempDir()); err == nil {
		t.Error("expected error for missing cases.json")
	}
}

// TestDatasetFingerprint_DetectsLabelChange verifies the fingerprint is stable
// for identical datasets and changes when a label changes, so an A/B comparison
// can detect that two scorecards were not computed on the same ground truth.
func TestDatasetFingerprint_DetectsLabelChange(t *testing.T) {
	base := &Dataset{Cases: []Case{
		{Name: "a", BuildPrefix: "logs/j/1/", TestName: "t", Labels: Labels{IsTransient: false}},
		{Name: "b", BuildPrefix: "logs/j/2/", TestName: "t", Labels: Labels{IsTransient: true}},
	}}
	// Same cases in a different order must hash identically.
	reordered := &Dataset{Cases: []Case{base.Cases[1], base.Cases[0]}}
	if base.Fingerprint() != reordered.Fingerprint() {
		t.Error("fingerprint must be order-independent")
	}
	// Flipping a label must change the fingerprint.
	changed := &Dataset{Cases: []Case{
		base.Cases[0],
		{Name: "b", BuildPrefix: "logs/j/2/", TestName: "t", Labels: Labels{IsTransient: false}},
	}}
	if base.Fingerprint() == changed.Fingerprint() {
		t.Error("fingerprint must change when a label changes")
	}
}

// TestDatasetFingerprint_DetectsArtifactChange verifies the fingerprint folds in
// artifact contents, so the same cases/labels over different recorded evidence
// do not look like the same dataset.
func TestDatasetFingerprint_DetectsArtifactChange(t *testing.T) {
	cases := []Case{{Name: "a", BuildPrefix: "logs/j/1/", TestName: "t", Labels: Labels{IsTransient: false}}}
	dirA := t.TempDir()
	if err := os.WriteFile(filepath.Join(dirA, "build-log.txt"), []byte("evidence one"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirB := t.TempDir()
	if err := os.WriteFile(filepath.Join(dirB, "build-log.txt"), []byte("evidence two"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Dataset{ArtifactRoot: dirA, Cases: cases}
	b := &Dataset{ArtifactRoot: dirB, Cases: cases}
	if a.Fingerprint() == b.Fingerprint() {
		t.Error("fingerprint must change when artifact contents change")
	}
}
