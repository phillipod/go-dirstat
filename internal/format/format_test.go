package format

import "testing"

func TestBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0B"},
		{1023, "1023B"},
		{1024, "1.00K"},
		{1536, "1.50K"},
		{1048576, "1.00M"},
		{12_582_912, "12.0M"},
		{524_288_000, "500M"},
		{1_073_741_824, "1.00G"},
		{1 << 40, "1.00T"},
		{1 << 50, "1.00P"},
		{1 << 60, "1.00E"},
		{-1, "?"},
	}
	for _, c := range cases {
		if got := Bytes(c.in); got != c.want {
			t.Errorf("Bytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSafeTextEscapesTerminalControlsAndInvalidUTF8(t *testing.T) {
	input := "snow 雪\ttab\nline\rreturn\x1b[31m" + string([]byte{0xff}) + "\u0085"
	want := `snow 雪\ttab\nline\rreturn\x1B[31m\xFF\x85`
	if got := SafeText(input); got != want {
		t.Fatalf("SafeText() = %q, want %q", got, want)
	}
}

func TestBar(t *testing.T) {
	full := Bar(1.0, 5)
	if full != "█████" {
		t.Errorf("full bar = %q, want █████", full)
	}
	empty := Bar(0, 5)
	if empty != "     " {
		t.Errorf("empty bar = %q, want 5 spaces", empty)
	}
	half := Bar(0.5, 4)
	if len([]rune(half)) != 4 {
		t.Errorf("half bar width = %d runes, want 4", len([]rune(half)))
	}
	if half != "██  " {
		t.Fatalf("half bar = %q, want %q", half, "██  ")
	}
	if Bar(0.0, 0) != "" {
		t.Error("zero-width bar should be empty string")
	}
}

func TestPctAndFrac(t *testing.T) {
	if got := Pct(250, 1000); got != 25 {
		t.Errorf("Pct(250,1000) = %v, want 25", got)
	}
	if got := Pct(10, 0); got != 0 {
		t.Errorf("Pct(10,0) = %v, want 0", got)
	}
	if got := Frac(3, 4); got != 0.75 {
		t.Errorf("Frac(3,4) = %v, want 0.75", got)
	}
	if got := Frac(10, 4); got != 1 { // clamped
		t.Errorf("Frac(10,4) = %v, want 1", got)
	}
}

func TestCount(t *testing.T) {
	if got := Count(1234567); got != "1,234,567" {
		t.Errorf("Count = %q, want 1,234,567", got)
	}
	if got := Count(0); got != "0" {
		t.Errorf("Count(0) = %q", got)
	}
}
