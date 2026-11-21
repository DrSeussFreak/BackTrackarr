package config

import "testing"

func TestEverySettingIsExplained(t *testing.T) {
	levels := map[string]bool{essential: true, recommended: true, optional: true, advanced: true}
	groups := map[string]bool{}
	for _, s := range Settings() {
		i := InfoFor(s)
		if i.What == "" || i.Needed == "" || !levels[i.Level] {
			t.Errorf("%s has no explanation: %+v", s.Key, i)
		}
		groups[s.Group] = true
	}
	for g := range groups {
		if GroupHelp[g] == "" {
			t.Errorf("section %q has no guidance", g)
		}
	}
}
