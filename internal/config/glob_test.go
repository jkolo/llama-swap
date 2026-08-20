package config

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGlob_StarCrossesSlash(t *testing.T) {
	re, err := CompileGlob("*")
	require.NoError(t, err)
	assert.IsType(t, &regexp.Regexp{}, re)
	assert.True(t, re.MatchString("z-ai/glm-4.7"))
	assert.True(t, re.MatchString("gpt-4o"))
}

func TestGlob_StarPrefix(t *testing.T) {
	re, err := CompileGlob("openai/*")
	require.NoError(t, err)
	assert.True(t, re.MatchString("openai/gpt-4o"))
	assert.True(t, re.MatchString("openai/gpt-4o-mini"))
	assert.False(t, re.MatchString("anthropic/claude"))
	// must be anchored: openai/ must be the prefix, not appear anywhere
	assert.False(t, re.MatchString("not-openai/gpt-4o"))
}

func TestGlob_StarSuffix(t *testing.T) {
	re, err := CompileGlob("*:free")
	require.NoError(t, err)
	assert.True(t, re.MatchString("z-ai/glm-4.7:free"))
	assert.False(t, re.MatchString("z-ai/glm-4.7"))
	assert.False(t, re.MatchString("z-ai/glm-4.7:free-extra"))
}

func TestGlob_QuestionMarkMatchesSingleChar(t *testing.T) {
	re, err := CompileGlob("model-?")
	require.NoError(t, err)
	assert.True(t, re.MatchString("model-1"))
	assert.False(t, re.MatchString("model-12"))
	assert.False(t, re.MatchString("model-"))
}

func TestGlob_AnchoredFullMatch(t *testing.T) {
	re, err := CompileGlob("gpt-4o")
	require.NoError(t, err)
	assert.True(t, re.MatchString("gpt-4o"))
	// must not match as a substring
	assert.False(t, re.MatchString("gpt-4o-mini"))
	assert.False(t, re.MatchString("not-gpt-4o"))
}

func TestGlob_RegexMetacharactersInLiteralAreEscaped(t *testing.T) {
	re, err := CompileGlob("gpt-3.5+turbo")
	require.NoError(t, err)
	// literal '.' must not act as regex "any character"
	assert.True(t, re.MatchString("gpt-3.5+turbo"))
	assert.False(t, re.MatchString("gpt-3X5+turbo"))
	// literal '+' must not act as regex "one or more"
	assert.False(t, re.MatchString("gpt-3.5turbo"))
}

func TestGlob_AnyLiteralInputCompiles(t *testing.T) {
	// Every rune outside of '*'/'?' is escaped via regexp.QuoteMeta before
	// being embedded, so no glob pattern a user could type - including raw
	// regex metacharacters, trailing backslashes, or unbalanced brackets -
	// can produce an invalid underlying regular expression. This is the
	// property that makes discovery.include/exclude safe to accept
	// unsandboxed strings from config without a separate validation pass.
	for _, pattern := range []string{
		`foo\`, `[unbalanced`, `(group`, `a{2,`, `$anchor^`, `back\slash`,
	} {
		_, err := CompileGlob(pattern)
		require.NoError(t, err, "pattern %q should compile", pattern)
	}
}

func TestGlob_CompileGlobs(t *testing.T) {
	res, err := CompileGlobs([]string{"openai/*", "*:free"})
	require.NoError(t, err)
	require.Len(t, res, 2)
	assert.True(t, res[0].MatchString("openai/gpt-4o"))
	assert.True(t, res[1].MatchString("z-ai/glm-4.7:free"))
}

func TestGlob_CompileGlobsEmpty(t *testing.T) {
	res, err := CompileGlobs(nil)
	require.NoError(t, err)
	assert.Empty(t, res)
}

func TestGlobsMatch_ExcludeWinsOverInclude(t *testing.T) {
	include, err := CompileGlobs([]string{"openai/*"})
	require.NoError(t, err)
	exclude, err := CompileGlobs([]string{"*:free"})
	require.NoError(t, err)

	assert.True(t, GlobsMatch("openai/gpt-4o", include, exclude))
	assert.False(t, GlobsMatch("openai/gpt-4o:free", include, exclude))
	assert.False(t, GlobsMatch("anthropic/claude", include, exclude))
}

func TestGlobsMatch_EmptyIncludeMeansAll(t *testing.T) {
	exclude, err := CompileGlobs([]string{"*:free"})
	require.NoError(t, err)

	assert.True(t, GlobsMatch("openai/gpt-4o", nil, exclude))
	assert.True(t, GlobsMatch("anthropic/claude", nil, exclude))
	assert.False(t, GlobsMatch("openai/gpt-4o:free", nil, exclude))
}
