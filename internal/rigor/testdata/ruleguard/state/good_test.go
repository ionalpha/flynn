package state

import (
	"sort"
	"testing"
	"time"
)

func TestFilesNamedAsTestsAreLeftAlone(t *testing.T) {
	s := []string{time.Now().Format(time.RFC3339Nano)}
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
}
