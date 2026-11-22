package chains

import "testing"

func TestLocationPatterns(t *testing.T) {
	path := []string{"House", "Bedroom", "Shelf"}
	for _, tc := range []struct {
		pattern string
		match   bool
	}{
		{"House.Bedroom.Shelf", true}, {"House.Bedroom", false}, {"House.Bedroom.**", true},
		{"**.Bedroom.**", true}, {"**.Shelf", true}, {"House.*.Shelf", true}, {"House.*", false},
		{"**.Bedroom.**.Shelf", true}, {"**.Garage.**", false}, {"house.**", false},
	} {
		filter, err := ParsePlacementFilter(map[string]any{"location": tc.pattern, "tag": "guest", "tags": []any{"battery"}})
		if err != nil || filter.Match(Placement{Location: path, Tags: []string{"battery", "guest"}}) != tc.match {
			t.Errorf("%s expected %v, got %v", tc.pattern, tc.match, err)
		}
		if filter.Match(Placement{Location: path, Tags: []string{"battery"}}) {
			t.Error("required tag was ignored")
		}
	}
	filter, err := ParsePlacementFilter(map[string]any{"location": []any{"House", "Shelf.1"}})
	if err != nil || !filter.Match(Placement{Location: []string{"House", "Shelf.1"}}) {
		t.Fatal("literal dot should match")
	}
}
