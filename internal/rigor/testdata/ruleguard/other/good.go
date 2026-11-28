// Package other stands for a package whose output order is not part of its
// contract, where sort.Slice is allowed.
package other

import "sort"

func order(s []string) {
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
}
