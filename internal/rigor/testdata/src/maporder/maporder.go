// Package maporder holds the loops the map-order check must report and the ones
// it must accept.
package maporder

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
)

type set map[string]int

func appendUnsorted(m map[string]int) []string {
	var out []string
	for k := range m { // want "random order"
		out = append(out, k)
	}
	return out
}

func iterator(m map[string]int) []string {
	var out []string
	for k := range maps.Keys(m) { // want "random order"
		out = append(out, k)
	}
	return out
}

func namedMap(m set, b *strings.Builder) {
	for k := range m { // want "random order"
		b.WriteString(k)
	}
}

func generic[M ~map[string]int](m M) {
	for k := range m { // want "random order"
		fmt.Println(k)
	}
}

func joined(m map[string]string) (s string) {
	for _, v := range m { // want "random order"
		s += v
	}
	return s
}

func floatSum(m map[string]float64) (sum float64) {
	for _, v := range m { // want "random order"
		sum += v
	}
	return sum
}

func firstMatch(m map[string]int) string {
	for k, v := range m { // want "random order"
		if v > 0 {
			return k
		}
	}
	return ""
}

func anyOne(m map[string]int) (picked string) {
	for k := range m { // want "random order"
		picked = k
		break
	}
	return picked
}

func sharedBucket(m map[string]int) map[int][]string {
	out := map[int][]string{}
	for k, v := range m { // want "random order"
		out[v] = append(out[v], k)
	}
	return out
}

func sortedWithTies(m map[string]int) []string {
	var out []string
	for k := range m { // want "random order"
		out = append(out, k)
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) < len(out[j]) })
	return out
}

func noReason(m map[string]int) {
	//ordered:ok
	for k := range m { // want "needs a reason"
		fmt.Println(k)
	}
}

func staleMark(m map[string]int) (n int) {
	//ordered:ok counting // want "marks no loop"
	for range m {
		n++
	}
	return n
}

func sortedAfter(m map[string]int) []string {
	var out []string
	for k, v := range m {
		if v == 0 {
			continue
		}
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func sortedKeys(m map[string]int) {
	for _, k := range slices.Sorted(maps.Keys(m)) {
		fmt.Println(k)
	}
}

func overSlice(s []string) {
	for _, v := range s {
		fmt.Println(v)
	}
}

func orderFree(m, other map[string]int, lists map[string][]int) (sum, high int, seen bool) {
	grouped := map[string][]int{}
	for k, v := range m {
		other[k] = v
		sum += v
		if v > high {
			high = v
		}
		switch {
		case v < 0:
			seen = true
			break
		default:
			delete(other, k)
		}
		grouped[k] = append(grouped[k], v)
	}
	for _, l := range lists {
		slices.Sort(l)
		total := 0
		for _, n := range l {
			total += n
		}
		sum += total
	}
	return sum, high, seen
}

func marked(m map[string]int) {
	//ordered:ok each line goes to its own stream
	for k := range m {
		fmt.Println(k)
	}
}
