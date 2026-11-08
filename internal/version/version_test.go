package version

import "testing"

func TestConstraints(t *testing.T) {
	for _, tc := range []struct {
		constraint string
		allows     []string
		refuses    []string
	}{
		{"", []string{"3.14.10", "2.7"}, nil},
		{"*", []string{"3.14.10"}, nil},
		{"3.14", []string{"3.14", "3.14.0", "3.14.10"}, []string{"3.13.9", "3.15"}},
		{"==3.14.*", []string{"3.14.10"}, []string{"3.15.0"}},
		{"3.x", []string{"3.0", "3.14.10"}, []string{"4.0", "2.7"}},
		{"=3.14.10", []string{"3.14.10"}, []string{"3.14.11"}},
		{"===3.14", []string{"3.14.0"}, []string{"3.14.1"}},
		{">=3.10,<3.13", []string{"3.10", "3.12.9"}, []string{"3.13.0", "3.9"}},
		{">=3.10 <3.13", []string{"3.11"}, []string{"3.14"}},
		{">= 3.10, < 3.13", []string{"3.11"}, []string{"3.14"}},
		{"^3.12", []string{"3.12.0", "3.99"}, []string{"3.11", "4.0"}},
		{"^0.2.3", []string{"0.2.9"}, []string{"0.3.0"}},
		{"~3.13", []string{"3.13.9"}, []string{"3.14"}},
		{"~3.13.2", []string{"3.13.2", "3.13.9"}, []string{"3.13.1", "3.14"}},
		{"~=3.12", []string{"3.12", "3.99"}, []string{"4.0", "3.11"}},
		{"~=3.12.1", []string{"3.12.5"}, []string{"3.13", "3.12.0"}},
		{"3.11 || 3.13", []string{"3.11.2", "3.13.0"}, []string{"3.12.0"}},
		{"3.10 - 3.12", []string{"3.10", "3.12"}, []string{"3.12.1", "3.9"}},
		{"!=3.13.0", []string{"3.13.1"}, []string{"3.13.0"}},
		{"v3.14", []string{"3.14.2"}, nil},
		{">3.14", []string{"3.14.1"}, []string{"3.14", "3.14.0"}},
	} {
		c, err := ParseConstraint(tc.constraint)
		if err != nil {
			t.Errorf("%q: %v", tc.constraint, err)
			continue
		}
		for _, v := range tc.allows {
			if !c.Allows(mustParse(t, v)) {
				t.Errorf("%q refuses %s", tc.constraint, v)
			}
		}
		for _, v := range tc.refuses {
			if c.Allows(mustParse(t, v)) {
				t.Errorf("%q allows %s", tc.constraint, v)
			}
		}
	}
}

func TestBadConstraints(t *testing.T) {
	for _, text := range []string{"python3", "3.14rc1", ">=", "~=3", "!=3.*", ">=3.*", "1.2.3.4.5", "3..4", "03.1", "3 ||", "+3"} {
		if _, err := ParseConstraint(text); err == nil {
			t.Errorf("%q parsed", text)
		}
	}
}

func mustParse(t *testing.T, text string) Version {
	t.Helper()
	v, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
