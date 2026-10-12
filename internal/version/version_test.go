package version

import "testing"

func TestFromModuleVersion(t *testing.T) {
	cases := []struct {
		mod, want string
	}{
		{"v0.58.1", "v0.58.1"},
		{"v1.2.3", "v1.2.3"},
		{"(devel)", "dev"},
		{"", "dev"},
		{"v0.58.2-0.20261010120000-abcdef123456", "dev"},
		{"v0.58.1+dirty", "dev"},
		{"v0.58", "dev"},
		{"v0..1", "dev"},
		{"0.58.1", "dev"},
	}
	for _, c := range cases {
		if got := fromModuleVersion("dev", c.mod); got != c.want {
			t.Errorf("fromModuleVersion(%q) = %q, want %q", c.mod, got, c.want)
		}
	}
}
