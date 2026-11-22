package chains

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Placement describes where a device lives and the groups it belongs to.
type Placement struct {
	Location []string `json:"location,omitempty"`
	Tags     []string `json:"tags,omitempty"`
}

func (p *Placement) Normalize() error {
	if err := validateSegments(p.Location, false); err != nil {
		return fmt.Errorf("location: %w", err)
	}
	if err := validateSegments(p.Tags, false); err != nil {
		return fmt.Errorf("tags: %w", err)
	}
	p.Tags = slices.Clone(p.Tags)
	slices.Sort(p.Tags)
	p.Tags = slices.Compact(p.Tags)
	return nil
}

func validateSegments(parts []string, pattern bool) error {
	if len(parts) > 32 {
		return fmt.Errorf("at most 32 segments or tags are supported")
	}
	for _, p := range parts {
		if pattern && (p == "*" || p == "**") {
			continue
		}
		if p == "" || len(p) > 128 || strings.TrimSpace(p) != p || !utf8.ValidString(p) || strings.IndexFunc(p, unicode.IsControl) >= 0 || p == "*" || p == "**" {
			return fmt.Errorf("invalid segment %q", p)
		}
	}
	return nil
}

type PlacementFilter struct {
	Location []string
	Tags     []string
}

func (f PlacementFilter) Active() bool { return len(f.Location) > 0 || len(f.Tags) > 0 }

func (f PlacementFilter) Match(p Placement) bool {
	if len(f.Location) > 0 && (len(p.Location) == 0 || !matchLocation(f.Location, p.Location)) {
		return false
	}
	for _, t := range f.Tags {
		if !slices.Contains(p.Tags, t) {
			return false
		}
	}
	return true
}

// matchLocation lets the last recursive wildcard consume more segments on mismatch.
func matchLocation(pattern, path []string) bool {
	i, j, wildcard, retry := 0, 0, -1, 0
	for j < len(path) {
		switch {
		case i < len(pattern) && pattern[i] == "**":
			wildcard, retry = i, j
			i++
		case i < len(pattern) && (pattern[i] == "*" || pattern[i] == path[j]):
			i++
			j++
		case wildcard >= 0:
			retry++
			i, j = wildcard+1, retry
		default:
			return false
		}
	}
	for i < len(pattern) && pattern[i] == "**" {
		i++
	}
	return i == len(pattern)
}

func ParsePlacementFilter(selectors map[string]any) (PlacementFilter, error) {
	var f PlacementFilter
	if v, ok := selectors["location"]; ok {
		var valid bool
		if path, ok := v.(string); ok {
			f.Location, valid = strings.Split(path, "."), true
		} else {
			f.Location, valid = StringParts(v)
		}
		if !valid || len(f.Location) == 0 {
			return f, fmt.Errorf("location needs a dotted path or a nonempty list")
		}
		if err := validateSegments(f.Location, true); err != nil {
			return f, err
		}
	}
	if v, ok := selectors["tags"]; ok {
		var valid bool
		f.Tags, valid = StringParts(v)
		if !valid || len(f.Tags) == 0 {
			return f, fmt.Errorf("tags needs a nonempty list")
		}
	}
	if v, ok := selectors["tag"]; ok {
		tag, valid := v.(string)
		if !valid {
			return f, fmt.Errorf("tag needs a string")
		}
		f.Tags = append(slices.Clone(f.Tags), tag)
	}
	return f, validateSegments(f.Tags, false)
}

// StringParts reads a string list from typed or JSON-decoded metadata.
func StringParts(value any) ([]string, bool) {
	switch values := value.(type) {
	case []string:
		return values, true
	case []any:
		parts := make([]string, len(values))
		for i, value := range values {
			var ok bool
			if parts[i], ok = value.(string); !ok {
				return nil, false
			}
		}
		return parts, true
	default:
		return nil, false
	}
}
