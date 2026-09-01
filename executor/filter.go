package executor

import (
	"fmt"
	"regexp"
	"strings"
)

// RegexPrefix marks a pattern as a regular expression rather than a glob.
const RegexPrefix = "re:"

// Pattern matches test identifiers of the form "Target/Class/method()".
//
// A pattern is either a glob or, with the "re:" prefix, a regular expression
// anchored at both ends. In a glob, "*" matches any run of characters including
// "/" and "?" matches exactly one character; everything else is literal. Globs
// deliberately let "*" cross path separators so that "*Flaky*" behaves the way
// people expect when they write it.
//
// Matching ignores a trailing "()" on either side, so "App/LoginTests/testA"
// and "App/LoginTests/testA()" select the same test.
type Pattern struct {
	raw string
	re  *regexp.Regexp
}

// ParsePattern compiles a single glob or "re:"-prefixed regular expression.
func ParsePattern(raw string) (Pattern, error) {
	if strings.TrimSpace(raw) == "" {
		return Pattern{}, fmt.Errorf("empty pattern")
	}
	expr := raw
	if rest, ok := strings.CutPrefix(raw, RegexPrefix); ok {
		expr = rest
		if strings.TrimSpace(expr) == "" {
			return Pattern{}, fmt.Errorf("empty regular expression in %q", raw)
		}
	} else {
		// Make the call parens optional so that a glob written either way
		// matches an identifier written either way.
		expr = globToRegexp(trimCallParens(raw)) + `(?:\(\))?`
	}

	re, err := regexp.Compile("^(?:" + expr + ")$")
	if err != nil {
		return Pattern{}, fmt.Errorf("invalid pattern %q: %w", raw, err)
	}
	return Pattern{raw: raw, re: re}, nil
}

// String returns the pattern as it was written.
func (p Pattern) String() string { return p.raw }

// Match reports whether the pattern selects the given test identifier.
func (p Pattern) Match(identifier string) bool {
	if p.re == nil {
		return false
	}
	return p.re.MatchString(identifier) || p.re.MatchString(trimCallParens(identifier))
}

// globToRegexp translates a glob into an unanchored regular expression.
func globToRegexp(glob string) string {
	var b strings.Builder
	for _, r := range glob {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	return b.String()
}

// trimCallParens drops a trailing "()" from a test identifier.
func trimCallParens(identifier string) string {
	return strings.TrimSuffix(identifier, "()")
}

// Patterns is a set of patterns matched as a union.
type Patterns []Pattern

// ParsePatterns compiles every pattern, reporting the first failure.
func ParsePatterns(raw []string) (Patterns, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	patterns := make(Patterns, 0, len(raw))
	for _, r := range raw {
		p, err := ParsePattern(r)
		if err != nil {
			return nil, err
		}
		patterns = append(patterns, p)
	}
	return patterns, nil
}

// MatchAny reports whether any pattern matches. An empty set matches nothing.
func (ps Patterns) MatchAny(identifier string) bool {
	for _, p := range ps {
		if p.Match(identifier) {
			return true
		}
	}
	return false
}

// Filter selects test identifiers by include and exclude patterns.
type Filter struct {
	Include Patterns
	Exclude Patterns
}

// NewFilter compiles a Filter from raw pattern strings.
func NewFilter(include, exclude []string) (*Filter, error) {
	inc, err := ParsePatterns(include)
	if err != nil {
		return nil, fmt.Errorf("tests.include: %w", err)
	}
	exc, err := ParsePatterns(exclude)
	if err != nil {
		return nil, fmt.Errorf("tests.exclude: %w", err)
	}
	return &Filter{Include: inc, Exclude: exc}, nil
}

// Keep reports whether the identifier survives the filter. An empty include set
// keeps everything; exclude is applied afterwards and wins.
func (f *Filter) Keep(identifier string) bool {
	if f == nil {
		return true
	}
	if len(f.Include) > 0 && !f.Include.MatchAny(identifier) {
		return false
	}
	return !f.Exclude.MatchAny(identifier)
}

// Apply splits identifiers into the kept and the dropped set, preserving order.
func (f *Filter) Apply(identifiers []string) (kept, dropped []string) {
	for _, id := range identifiers {
		if f.Keep(id) {
			kept = append(kept, id)
			continue
		}
		dropped = append(dropped, id)
	}
	return kept, dropped
}

// validatePatterns compiles patterns purely to surface errors, attributing them
// to the config field they came from.
func validatePatterns(field string, raw []string) error {
	if _, err := ParsePatterns(raw); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	return nil
}
