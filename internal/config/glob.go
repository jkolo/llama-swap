package config

import (
	"fmt"
	"regexp"
	"strings"
)

// CompileGlob compiles a single glob pattern into an anchored regular
// expression. Unlike path.Match, '*' matches any sequence of characters
// including '/', since model IDs frequently contain slashes (e.g.
// "z-ai/glm-4.7"). '?' matches exactly one character. All other characters
// are treated literally, including regular expression metacharacters.
func CompileGlob(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")

	// Accumulate consecutive literal runes so they can be escaped together;
	// flush before/after each glob metacharacter.
	var literal strings.Builder
	flush := func() {
		if literal.Len() > 0 {
			b.WriteString(regexp.QuoteMeta(literal.String()))
			literal.Reset()
		}
	}

	for _, r := range pattern {
		switch r {
		case '*':
			flush()
			b.WriteString(".*")
		case '?':
			flush()
			b.WriteString(".")
		default:
			literal.WriteRune(r)
		}
	}
	flush()
	b.WriteString("$")

	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("invalid glob pattern %q: %w", pattern, err)
	}
	return re, nil
}

// CompileGlobs compiles a slice of glob patterns, returning an error naming
// the first pattern that fails to compile.
func CompileGlobs(patterns []string) ([]*regexp.Regexp, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := CompileGlob(p)
		if err != nil {
			return nil, err
		}
		compiled = append(compiled, re)
	}
	return compiled, nil
}

// GlobsMatch reports whether name should be kept, given a peer's include and
// exclude glob lists. An empty include list means "include everything".
// exclude always wins over include.
func GlobsMatch(name string, include, exclude []*regexp.Regexp) bool {
	for _, re := range exclude {
		if re.MatchString(name) {
			return false
		}
	}
	if len(include) == 0 {
		return true
	}
	for _, re := range include {
		if re.MatchString(name) {
			return true
		}
	}
	return false
}
