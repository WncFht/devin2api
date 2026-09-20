package selfupdate

import "testing"

func TestUpdateAvailable(t *testing.T) {
	cases := []struct {
		name    string
		latest  string
		current string
		want    bool
	}{
		{"same tag", "v0.16.0", "v0.16.0", false},
		{"newer tag", "v0.16.1", "v0.16.0", true},
		{"newer minor", "v0.17.0", "v0.16.0", true},
		{"older tag", "v0.15.0", "v0.16.0", false},
		{"ahead of tag", "v0.16.0", "v0.16.0-19-g8590aec", false},
		{"ahead of tag dirty", "v0.16.0", "v0.16.0-19-g8590aec-dirty", false},
		{"release newer than describe base", "v0.16.1", "v0.16.0-19-g8590aec", true},
		{"behind describe base", "v0.16.0", "v0.15.0-218-g94fdfe3", true},
		{"dirty at tag", "v0.16.0", "v0.16.0-dirty", false},
		{"dirty at tag newer release", "v0.16.1", "v0.16.0-dirty", true},
		{"dev build", "v0.16.0", "dev", true},
		{"bare sha current", "v0.16.0", "8590aec", true},
		{"empty latest", "", "v0.16.0", false},
		{"no v prefix", "0.16.1", "v0.16.0", true},
		{"rc base behind release", "v0.16.0", "v0.16.0-rc.1-3-g8590aec", true},
		{"running newer rc", "v0.16.0", "v0.17.0-rc.1", false},
		{"nonstandard latest falls back", "nightly-20260920", "v0.16.0", true},
		{"nonstandard latest equal", "nightly-20260920", "nightly-20260920", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := updateAvailable(c.latest, c.current); got != c.want {
				t.Errorf("updateAvailable(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
			}
		})
	}
}

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.0.0", "v1.0.0", 0},
		{"v1.0.0", "v1.0.1", -1},
		{"v1.2.0", "v1.10.0", -1},
		{"v2.0.0", "v1.9.9", 1},
		{"v1.0.0-rc.1", "v1.0.0", -1},
		{"v1.0.0-alpha", "v1.0.0-alpha.1", -1},
		{"v1.0.0-alpha.1", "v1.0.0-alpha.beta", -1},
		{"v1.0.0-beta", "v1.0.0-beta.2", -1},
		{"v1.0.0-beta.2", "v1.0.0-beta.11", -1},
		{"v1.0.0+build5", "v1.0.0", 0},
		{"v1.0.0-rc.1+build", "v1.0.0-rc.1", 0},
	}
	for _, c := range cases {
		a, aok := parseSemver(c.a)
		b, bok := parseSemver(c.b)
		if !aok || !bok {
			t.Fatalf("parse failed: %q(%v) %q(%v)", c.a, aok, c.b, bok)
		}
		if got := compareSemver(a, b); got != c.want {
			t.Errorf("compareSemver(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
