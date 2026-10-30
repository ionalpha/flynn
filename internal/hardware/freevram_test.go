package hardware

import "testing"

func TestParseFreeVRAM(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want int64
		ok   bool
	}{
		{"one gpu", "15360\n", 15360 * 1024 * 1024, true},
		{"first row of two, with blank lines and CRLF", "\n 1024 \r\n2048\n", 1024 * 1024 * 1024, true},
		{"empty output", "", 0, false},
		{"hidden figure", "N/A\n", 0, false},
		{"negative", "-5\n", 0, false},
		{"unusable first row is not skipped", "N/A\n2048\n", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseFreeVRAM(tc.in)
			if got != tc.want || ok != tc.ok {
				t.Errorf("parseFreeVRAM(%q) = %d, %v; want %d, %v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}
