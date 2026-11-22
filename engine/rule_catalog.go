package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	chains "chains-automation"
	"chains-automation/rules"
)

const maxRuleSource = 128 << 10

type ruleSource struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Origin  string `json:"origin"`
	Source  string `json:"source,omitempty"`
}

type ruleCatalog struct {
	mu       sync.Mutex
	dir      string
	revision string
	entries  []ruleSource
}

type ruleManifest struct {
	Version int          `json:"version"`
	Rules   []ruleSource `json:"rules"`
}

func openRuleCatalog(dir string, fixed map[string]string) (*ruleCatalog, error) {
	c := &ruleCatalog{dir: dir, revision: chains.ID(), entries: []ruleSource{}}
	for name, source := range fixed {
		c.entries = append(c.entries, ruleSource{ID: "file-" + name, Name: name, Source: source, Enabled: true, Origin: "file"})
	}
	slices.SortFunc(c.entries, func(a, b ruleSource) int { return strings.Compare(a.Name, b.Name) })
	if dir == "" {
		return c, nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, "CURRENT"))
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	generation := strings.TrimSpace(string(raw))
	if !ruleID(generation) {
		return nil, errors.New("invalid rules generation")
	}
	c.revision = generation
	path := filepath.Join(dir, generation)
	manifestRaw, err := os.ReadFile(filepath.Join(path, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var manifest ruleManifest
	if err = json.Unmarshal(manifestRaw, &manifest); err != nil {
		return nil, err
	}
	if manifest.Version != 1 {
		return nil, errors.New("unsupported rule manifest version")
	}
	ids := map[string]bool{}
	names := map[string]bool{}
	for _, item := range c.entries {
		names[item.Name] = true
	}
	for _, item := range manifest.Rules {
		if !ruleID(item.ID) || !validRuleName(item.Name) || ids[item.ID] || names[item.Name] {
			return nil, fmt.Errorf("invalid or duplicate stored rule %q", item.Name)
		}
		ids[item.ID], names[item.Name] = true, true
		data, err := os.ReadFile(filepath.Join(path, item.ID+".chains"))
		if err != nil {
			return nil, err
		}
		item.Source, item.Origin = string(data), "web"
		c.entries = append(c.entries, item)
	}
	return c, nil
}

func ruleID(id string) bool {
	return len(id) == 32 && strings.IndexFunc(id, func(c rune) bool { return !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') }) < 0
}

func validRuleName(name string) bool {
	return name != "" && len(name) <= 128 && strings.TrimSpace(name) == name && utf8.ValidString(name) &&
		strings.IndexFunc(name, func(r rune) bool { return r < 32 || r == 127 || r == '/' || r == '\\' }) < 0
}

// persist commits the new generation by replacing CURRENT on the same filesystem.
func (c *ruleCatalog) persist(entries []ruleSource, generation string) (bool, error) {
	if err := os.MkdirAll(c.dir, 0700); err != nil {
		return false, err
	}
	dir := filepath.Join(c.dir, generation)
	if err := os.Mkdir(dir, 0700); err != nil {
		return false, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(dir)
		}
	}()
	manifest := ruleManifest{Version: 1, Rules: []ruleSource{}}
	for _, item := range entries {
		if item.Origin != "web" {
			continue
		}
		if err := writeRuleFile(filepath.Join(dir, item.ID+".chains"), []byte(item.Source)); err != nil {
			return false, err
		}
		item.Source = ""
		manifest.Rules = append(manifest.Rules, item)
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return false, err
	}
	if err = writeRuleFile(filepath.Join(dir, "manifest.json"), raw); err != nil {
		return false, err
	}
	if err = syncRuleDir(dir); err != nil {
		return false, err
	}
	old, _ := os.ReadFile(filepath.Join(c.dir, "CURRENT"))
	tmp := filepath.Join(c.dir, "."+generation)
	defer os.Remove(tmp)
	if err = writeRuleFile(tmp, []byte(generation+"\n")); err != nil {
		return false, err
	}
	if err = os.Rename(tmp, filepath.Join(c.dir, "CURRENT")); err != nil {
		return false, err
	}
	committed = true
	if err = syncRuleDir(c.dir); err != nil {
		return true, fmt.Errorf("generation selected, directory sync: %w", err)
	}
	if previous := strings.TrimSpace(string(old)); ruleID(previous) && previous != generation {
		_ = os.RemoveAll(filepath.Join(c.dir, previous))
	}
	return true, nil
}

func writeRuleFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

func syncRuleDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func parseWebRule(name, source string) (rules.Rule, error) {
	if !validRuleName(name) {
		return rules.Rule{}, errors.New("enter a rule name of up to 128 bytes without path separators")
	}
	if len(source) > maxRuleSource || !utf8.ValidString(source) {
		return rules.Rule{}, errors.New("rule source must be UTF-8 and at most 128 KiB")
	}
	return rules.Parse(name, source)
}
