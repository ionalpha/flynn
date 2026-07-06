package watch

import "testing"

func TestClear(t *testing.T) {
	cases := []struct {
		name        string
		content     string
		line        int
		wantChanged bool
		want        string
	}{
		{
			name:        "keep code drop comment",
			content:     "a := 1\nb := 2 // ai! rename\nc := 3\n",
			line:        2,
			wantChanged: true,
			want:        "a := 1\nb := 2\nc := 3\n",
		},
		{
			name:        "drop whole comment line",
			content:     "a := 1\n// ai! write docs\nc := 3\n",
			line:        2,
			wantChanged: true,
			want:        "a := 1\nc := 3\n",
		},
		{
			name:        "preserve crlf keeping code",
			content:     "a := 1\r\nb := 2 // ai! go\r\nc := 3\r\n",
			line:        2,
			wantChanged: true,
			want:        "a := 1\r\nb := 2\r\nc := 3\r\n",
		},
		{
			name:        "no marker on line is a noop",
			content:     "a := 1\nb := 2\n",
			line:        2,
			wantChanged: false,
			want:        "a := 1\nb := 2\n",
		},
		{
			name:        "line out of range",
			content:     "a := 1\n",
			line:        9,
			wantChanged: false,
			want:        "a := 1\n",
		},
		{
			name:        "line zero",
			content:     "// ai! x\n",
			line:        0,
			wantChanged: false,
			want:        "// ai! x\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := Clear([]byte(tc.content), tc.line)
			if changed != tc.wantChanged {
				t.Fatalf("changed = %v, want %v", changed, tc.wantChanged)
			}
			if string(got) != tc.want {
				t.Errorf("content =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}
