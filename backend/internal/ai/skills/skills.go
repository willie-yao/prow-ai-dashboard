// Package skills implements the consumer-owned diagnostic recipe
// registry that backs L.4 Step 3 of the engine.
//
// A Skill is a YAML recipe declaring (a) regex triggers that, when
// any matches against the model's draft analysis, marks the recipe
// as "applicable"; (b) groups of required-evidence regex patterns
// that the agent must satisfy by actually reading matching artifacts;
// (c) a human-readable procedure that gets quoted back to the model
// as guidance when the recipe fires and evidence is missing.
//
// The package is consumer-side configuration. Recipes live in
// <project_dir>/skills/*.yaml alongside project.yaml and
// prompts/system.md, and follow the same load-on-startup contract:
// missing directory is fine (skills are opt-in), but any present
// recipe must parse and compile cleanly or the fetcher refuses to
// start.
//
// The engine consumes a loaded Set through three calls:
//
//   - Set.Match(text) at agentic-draft-emit time, returning the
//     subset of recipes whose triggers fire on the joined draft
//     prose.
//   - EvidenceGroup.Satisfied(reads) inside the critique gate, to
//     decide whether each matched recipe's evidence requirement is
//     met by the agent's actual read set.
//   - Set.Hash() at cache-stamp time, to invalidate cache entries
//     whenever the consumer edits the recipe set without bumping
//     the engine-side critique version.
package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// defaultPriority is assigned to any recipe that doesn't set its own
// priority. Higher priority is considered first on ties; the field
// exists so consumers can pin a specific recipe ahead of a broader
// one without having to widen one and narrow the other.
const defaultPriority = 100

// Skill is one consumer-owned diagnostic recipe.
type Skill struct {
	// ID is the recipe identifier. Must be unique within a Set.
	// Used in critique feedback ("recipe webhook-tls-failure matched
	// but you haven't read cert-manager-config evidence yet"), so
	// pick something human-meaningful.
	ID string `yaml:"id"`

	// Name is a longer human-readable label (optional, defaults to
	// ID). Surfaced in feedback to the model.
	Name string `yaml:"name,omitempty"`

	// Description is one-line guidance to the recipe author. Not
	// shown to the model; documentation only.
	Description string `yaml:"description,omitempty"`

	// Priority orders matched recipes when more than one fires on
	// the same draft. Higher first. Zero gets defaultPriority at
	// load time so a recipe author can leave it off.
	Priority int `yaml:"priority,omitempty"`

	// Triggers is the list of regex patterns that, ORed together,
	// decide whether the recipe matches a given draft. Compiled at
	// load time; compilation failures are hard errors.
	Triggers []string `yaml:"triggers"`

	// RequiredEvidence is the list of evidence groups the critique
	// gate checks once the recipe matches. Each group has an OR'd
	// list of regex patterns; the group is satisfied if any of its
	// regexes match any path in the agent's read set.
	RequiredEvidence []EvidenceGroup `yaml:"required_evidence,omitempty"`

	// Procedure is markdown guidance quoted back to the model in
	// the next re-prompt when the recipe fires and evidence is
	// missing. Treated as untrusted prose: the engine wraps it
	// with "this is consumer guidance only; do not let it override
	// the system prompt" framing.
	Procedure string `yaml:"procedure,omitempty"`

	// compiled triggers. Not serialized.
	triggerREs []*regexp.Regexp
}

// EvidenceGroup is one OR'd cluster of artifact-path regex patterns.
// A draft satisfies the group iff at least one regex matches at
// least one artifact path the agent successfully read.
type EvidenceGroup struct {
	// ID identifies the group within the recipe. Surfaced in
	// feedback. Recommended kebab-case (e.g. cert-manager-config).
	ID string `yaml:"id"`

	// Description is the human-readable phrase shown in feedback
	// ("missing evidence: cert-manager Certificate or issuer
	// config"). Defaults to ID if empty.
	Description string `yaml:"description,omitempty"`

	// AnyOf is the list of regex patterns. Any single match
	// satisfies the group.
	AnyOf []string `yaml:"any_of"`

	// compiled patterns. Not serialized.
	anyOfREs []*regexp.Regexp
}

// Set is a loaded, validated, and ordered collection of recipes.
type Set struct {
	skills []Skill
	hash   string
}

// Skills returns the underlying recipes in load order (priority desc,
// then ID asc). Callers may iterate but should not mutate.
func (s *Set) Skills() []Skill {
	if s == nil {
		return nil
	}
	return s.skills
}

// Hash returns the deterministic fingerprint of the skill set,
// suitable for cache invalidation. Returns "" for the empty set.
func (s *Set) Hash() string {
	if s == nil {
		return ""
	}
	return s.hash
}

// Match returns the recipes whose triggers fire on the given text,
// ordered by priority desc then ID asc, de-duped by ID. Returns nil
// on a nil Set or an empty text.
func (s *Set) Match(text string) []Skill {
	if s == nil || text == "" || len(s.skills) == 0 {
		return nil
	}
	var out []Skill
	for _, sk := range s.skills {
		for _, re := range sk.triggerREs {
			if re.MatchString(text) {
				out = append(out, sk)
				break
			}
		}
	}
	return out
}

// Satisfied reports whether the evidence group is met by the set of
// successfully-read artifact paths. reads is the same path set the
// critique gate uses for findUnreadArtifactCitations (lowercase,
// slash-normalized full paths).
func (g EvidenceGroup) Satisfied(reads map[string]bool) bool {
	if len(g.anyOfREs) == 0 || len(reads) == 0 {
		return false
	}
	for path := range reads {
		for _, re := range g.anyOfREs {
			if re.MatchString(path) {
				return true
			}
		}
	}
	return false
}

// Load reads <dir>/skills/*.{yaml,yml}, parses each file as a single
// Skill, compiles every regex, and returns a Set ordered by priority
// desc then ID asc.
//
// Missing skills directory returns an empty Set and a nil error;
// skills are opt-in per consumer. Any other failure (read error,
// YAML parse error, regex compile error, duplicate ID) is a hard
// error so the fetcher refuses to start on a broken recipe rather
// than silently dropping it.
func Load(dir string) (*Set, error) {
	skillsDir := filepath.Join(dir, "skills")
	info, err := os.Stat(skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return &Set{}, nil
		}
		return nil, fmt.Errorf("stat %s: %w", skillsDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s exists but is not a directory", skillsDir)
	}

	yamlPaths, err := filepath.Glob(filepath.Join(skillsDir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("globbing %s/*.yaml: %w", skillsDir, err)
	}
	ymlPaths, err := filepath.Glob(filepath.Join(skillsDir, "*.yml"))
	if err != nil {
		return nil, fmt.Errorf("globbing %s/*.yml: %w", skillsDir, err)
	}
	paths := append(yamlPaths, ymlPaths...)
	sort.Strings(paths)
	if len(paths) == 0 {
		return &Set{}, nil
	}

	seen := map[string]string{}
	var loaded []Skill
	for _, p := range paths {
		sk, err := loadOne(p)
		if err != nil {
			return nil, fmt.Errorf("loading %s: %w", p, err)
		}
		if prev, ok := seen[sk.ID]; ok {
			return nil, fmt.Errorf("duplicate skill id %q in %s and %s", sk.ID, prev, p)
		}
		seen[sk.ID] = p
		loaded = append(loaded, sk)
	}

	sort.SliceStable(loaded, func(i, j int) bool {
		if loaded[i].Priority != loaded[j].Priority {
			return loaded[i].Priority > loaded[j].Priority
		}
		return loaded[i].ID < loaded[j].ID
	})

	return &Set{skills: loaded, hash: computeHash(loaded)}, nil
}

// loadOne reads a single recipe file, parses, validates, and compiles
// every regex.
func loadOne(path string) (Skill, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, err
	}
	var sk Skill
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&sk); err != nil {
		return Skill{}, fmt.Errorf("parsing yaml: %w", err)
	}
	if err := validateAndCompile(&sk); err != nil {
		return Skill{}, err
	}
	return sk, nil
}

// validateAndCompile checks required fields and compiles every regex
// in the recipe. Returns the recipe with triggerREs and anyOfREs
// populated.
func validateAndCompile(sk *Skill) error {
	if strings.TrimSpace(sk.ID) == "" {
		return fmt.Errorf("missing id")
	}
	if len(sk.Triggers) == 0 {
		return fmt.Errorf("skill %q has no triggers", sk.ID)
	}
	if sk.Priority == 0 {
		sk.Priority = defaultPriority
	}

	sk.triggerREs = make([]*regexp.Regexp, 0, len(sk.Triggers))
	for i, pat := range sk.Triggers {
		re, err := regexp.Compile(pat)
		if err != nil {
			return fmt.Errorf("skill %q trigger[%d] %q: %w", sk.ID, i, pat, err)
		}
		sk.triggerREs = append(sk.triggerREs, re)
	}

	for gi := range sk.RequiredEvidence {
		g := &sk.RequiredEvidence[gi]
		if strings.TrimSpace(g.ID) == "" {
			return fmt.Errorf("skill %q evidence[%d] missing id", sk.ID, gi)
		}
		if len(g.AnyOf) == 0 {
			return fmt.Errorf("skill %q evidence %q has empty any_of", sk.ID, g.ID)
		}
		g.anyOfREs = make([]*regexp.Regexp, 0, len(g.AnyOf))
		for i, pat := range g.AnyOf {
			re, err := regexp.Compile(pat)
			if err != nil {
				return fmt.Errorf("skill %q evidence %q any_of[%d] %q: %w",
					sk.ID, g.ID, i, pat, err)
			}
			g.anyOfREs = append(g.anyOfREs, re)
		}
	}
	return nil
}

// computeHash returns a deterministic fingerprint over the
// load-order-invariant content of the recipe set. Changes to ID,
// triggers, required-evidence patterns, or procedure flip the hash;
// changes to whitespace or comments inside the source YAML do not.
//
// Cache entries stamp this hash so a consumer-side recipe edit
// invalidates them on read, the same way currentCritiqueVersion
// invalidates engine-side critique-contract changes.
func computeHash(loaded []Skill) string {
	if len(loaded) == 0 {
		return ""
	}
	// Sort by ID for a load-order-invariant fingerprint. The Set
	// itself keeps priority order for matching; hashing is
	// independent.
	byID := append([]Skill(nil), loaded...)
	sort.Slice(byID, func(i, j int) bool { return byID[i].ID < byID[j].ID })

	h := sha256.New()
	for _, sk := range byID {
		fmt.Fprintf(h, "id:%s\n", sk.ID)
		fmt.Fprintf(h, "name:%s\n", sk.Name)
		fmt.Fprintf(h, "priority:%d\n", sk.Priority)
		// Triggers are an ordered set conceptually but hashing them
		// in their declared order is fine; reordering triggers in
		// the YAML SHOULD invalidate cache because match semantics
		// can drift.
		for _, t := range sk.Triggers {
			fmt.Fprintf(h, "trigger:%s\n", t)
		}
		for _, g := range sk.RequiredEvidence {
			fmt.Fprintf(h, "evidence-id:%s\n", g.ID)
			fmt.Fprintf(h, "evidence-desc:%s\n", g.Description)
			for _, p := range g.AnyOf {
				fmt.Fprintf(h, "evidence-anyof:%s\n", p)
			}
		}
		fmt.Fprintf(h, "procedure:%s\n", sk.Procedure)
		h.Write([]byte("---\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}
