package learnreplay

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The committed message words are the code's: a new message word in the
// sources fails here until `make learner-message-words` writes the file.
func TestMessageWords_AreCurrent(t *testing.T) {
	got, err := MessageWords(moduleRoot(t))
	require.NoError(t, err)
	path := "message_words.txt"
	if os.Getenv("LEARNREPLAY_WRITE_MESSAGE_WORDS") == "1" {
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
		t.Logf("wrote %d words", strings.Count(got, "\n"))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, strings.ReplaceAll(string(want), "\r\n", "\n"), got, "message_words.txt is stale: run make learner-message-words")
	assert.Greater(t, len(messageWords), 1000, "the words were not embedded")
	for _, w := range []string{"variant", "line", "no", "prometheus"} {
		assert.True(t, messageWords[w], "%q is not a message word", w)
	}
}
