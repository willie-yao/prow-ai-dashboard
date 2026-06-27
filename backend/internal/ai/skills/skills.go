// Package skills implements the consumer-owned diagnostic recipe registry.
//
// A Skill is a YAML recipe with regex triggers, required-evidence groups, and a
// human-readable procedure quoted back to the model when evidence is missing.
//
// Recipes live in <project_dir>/skills/*.yaml. The directory is optional;
// any present recipe must parse and compile cleanly or Load returns an error.
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
// priority. Higher priority is preferred on ties.
const defaultPriority = 100

// Skill is one consumer-owned diagnostic recipe.
type Skill struct {
	// ID is the recipe identifier; must be unique within a Set. Surfaced
	// in critique feedback, so pick something human-meaningful.
	ID string `yaml:"id"`

	// Name is an optional longer label. Defaults to ID.
	Name string `yaml:"name,omitempty"`

	// Description is one-line guidance for the recipe author. Not shown
	// to the model.
	Description string `yaml:"description,omitempty"`

	// Priority orders matched recipes when more than one fires on the
	// same draft. Higher first; defaults to defaultPriority.
	Priority int `yaml:"priority,omitempty"`

	// Triggers is the list of regex patterns ORed together to decide
	// whether the recipe matches a given draft. Compiled at Load time.
	Triggers []string `yaml:"triggers"`

	// RequiredEvidence is the list of evidence groups the critique gate
	// checks once the recipe matches. Each group is satisfied if any of
	// its any_of regexes matches any path the agent successfully read.
	RequiredEvidence []EvidenceGroup `yaml:"required_evidence,omitempty"`

	// Procedure is markdown guidance quoted back to the model when the
	// recipe fires and evidence is missing. Treated as untrusted prose;
	// the engine wraps it with "consumer guidance only" framing.
	Procedure string `yaml:"procedure,omitempty"`

	// compiled triggers. Not serialized.
	triggerREs []*regexp.Regexp
}

// EvidenceGroup is one OR'd cluster of artifact-path regex patterns. A
// draft satisfies the group iff at least one regex matches at least one
// artifact path the agent successfully read.
type EvidenceGroup struct {
	// ID identifies the group within the recipe. Surfaced in feedback;
	// recommended kebab-case, such as cert-manager-config.
	ID string `yaml:"id"`

	// Description is the human-readable phrase shown in feedback.
	// Defaults to ID if empty.
	Description string `yaml:"description,omitempty"`

	// AnyOf is the list of regex patterns. Any single match satisfies
	// the group.
	AnyOf []string `yaml:"any_of"`

	// compiled patterns. Not serialized.
	anyOfREs []*regexp.Regexp
}

// Set is a loaded, validated, and ordered collection of recipes.
type Set struct {
	skills []Skill
	hash   string
}

// Skills returns recipes in load order, priority desc then ID asc. Callers may
// iterate but should not mutate.
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
// critique gate uses for findUnreadArtifactCitations: lowercase,
// slash-normalized full paths.
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

// Load reads <dir>/skills/*.{yaml,yml}, parses each as a single Skill,
// compiles every regex, and returns a Set ordered by priority desc then
// ID asc. A missing directory returns an empty Set. Read errors, YAML
// parse errors, regex compile errors, and duplicate IDs are hard errors.
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
	sk, err := ParseAndValidate(data)
	if err != nil {
		return Skill{}, err
	}
	return sk, nil
}

// ParseAndValidate decodes one recipe from YAML, then validates and compiles
// every regex. It is the single-recipe entry point used by callers that
// generate a recipe and need to reject an invalid draft before writing it.
func ParseAndValidate(data []byte) (Skill, error) {
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

// computeHash returns a deterministic fingerprint over the load-order-
// invariant content of the recipe set. Changes to ID, triggers,
// required-evidence patterns, or procedure flip the hash; whitespace or
// comment changes in the source YAML do not.
func computeHash(loaded []Skill) string {
	if len(loaded) == 0 {
		return ""
	}
	// Sort by ID for a load-order-invariant fingerprint.
	byID := append([]Skill(nil), loaded...)
	sort.Slice(byID, func(i, j int) bool { return byID[i].ID < byID[j].ID })

	h := sha256.New()
	for _, sk := range byID {
		fmt.Fprintf(h, "id:%s\n", sk.ID)
		fmt.Fprintf(h, "name:%s\n", sk.Name)
		fmt.Fprintf(h, "priority:%d\n", sk.Priority)
		// Trigger order matters for match semantics, so it must
		// affect the hash too.
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
