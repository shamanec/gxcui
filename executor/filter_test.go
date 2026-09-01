package executor

import (
	"strings"
	"testing"
)

func TestPatternMatch(t *testing.T) {
	tests := []struct {
		pattern    string
		identifier string
		want       bool
	}{
		{"App/LoginTests/testA()", "App/LoginTests/testA()", true},
		// A trailing "()" is optional on either side.
		{"App/LoginTests/testA", "App/LoginTests/testA()", true},
		{"App/LoginTests/testA()", "App/LoginTests/testA", true},
		{"App/LoginTests", "App/LoginTests/testA()", false},
		{"App/LoginTests/*", "App/LoginTests/testA()", true},
		{"App/*", "App/LoginTests/testA()", true},
		// "*" crosses "/" so that a bare substring pattern behaves as written.
		{"*Login*", "App/LoginTests/testA()", true},
		{"*Logout*", "App/LoginTests/testA()", false},
		{"App/LoginTests/test?()", "App/LoginTests/testA()", true},
		{"App/LoginTests/test?()", "App/LoginTests/testAB()", false},
		// Glob metacharacters in the identifier stay literal.
		{"App/Login.Tests/testA()", "App/LoginXTests/testA()", false},
		{"re:App/.*Tests/test[AB]\\(\\)", "App/LoginTests/testA()", true},
		{"re:App/.*Tests/test[AB]\\(\\)", "App/LoginTests/testC()", false},
		// A regex is anchored, so a partial match does not select.
		{"re:LoginTests", "App/LoginTests/testA()", false},
	}

	for _, tt := range tests {
		p, err := ParsePattern(tt.pattern)
		if err != nil {
			t.Fatalf("ParsePattern(%q) error = %v", tt.pattern, err)
		}
		if got := p.Match(tt.identifier); got != tt.want {
			t.Errorf("ParsePattern(%q).Match(%q) = %v, want %v", tt.pattern, tt.identifier, got, tt.want)
		}
	}
}

func TestParsePatternRejectsBadInput(t *testing.T) {
	for _, raw := range []string{"", "   ", "re:", "re:["} {
		if _, err := ParsePattern(raw); err == nil {
			t.Errorf("ParsePattern(%q) error = nil, want an error", raw)
		}
	}
}

func TestFilterApply(t *testing.T) {
	all := []string{
		"App/LoginTests/testA()",
		"App/LoginTests/testB()",
		"App/CheckoutTests/testC()",
		"App/FlakyTests/testD()",
	}

	tests := []struct {
		name    string
		include []string
		exclude []string
		want    []string
	}{
		{
			name: "no patterns keeps everything",
			want: all,
		},
		{
			name:    "include narrows",
			include: []string{"App/LoginTests/*"},
			want:    []string{"App/LoginTests/testA()", "App/LoginTests/testB()"},
		},
		{
			name:    "exclude drops",
			exclude: []string{"App/FlakyTests/*"},
			want: []string{
				"App/LoginTests/testA()",
				"App/LoginTests/testB()",
				"App/CheckoutTests/testC()",
			},
		},
		{
			name:    "exclude wins over include",
			include: []string{"App/*"},
			exclude: []string{"*Flaky*"},
			want: []string{
				"App/LoginTests/testA()",
				"App/LoginTests/testB()",
				"App/CheckoutTests/testC()",
			},
		},
		{
			name:    "includes union",
			include: []string{"App/LoginTests/testA", "App/CheckoutTests/*"},
			want:    []string{"App/LoginTests/testA()", "App/CheckoutTests/testC()"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := NewFilter(tt.include, tt.exclude)
			if err != nil {
				t.Fatalf("NewFilter() error = %v", err)
			}
			kept, dropped := f.Apply(all)
			if strings.Join(kept, ",") != strings.Join(tt.want, ",") {
				t.Errorf("kept = %v, want %v", kept, tt.want)
			}
			if len(kept)+len(dropped) != len(all) {
				t.Errorf("kept %d + dropped %d != %d input tests", len(kept), len(dropped), len(all))
			}
		})
	}
}

func TestNilFilterKeepsEverything(t *testing.T) {
	var f *Filter
	if !f.Keep("App/LoginTests/testA()") {
		t.Error("a nil Filter should keep every test")
	}
}
