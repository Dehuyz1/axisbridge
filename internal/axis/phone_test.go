package axis

import "testing"

func TestNormalizePhone(t *testing.T) {
	cases := map[string]string{
		"083131234567":       "6283131234567",
		"+62 831-3123-4567":  "6283131234567",
		"6283131234567":      "6283131234567",
		"831 3123 4567":      "6283131234567",
		"":                   "",
		"abc":                "",
		"0838":               "62838",
	}
	for in, want := range cases {
		if got := NormalizePhone(in); got != want {
			t.Errorf("NormalizePhone(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsAxis(t *testing.T) {
	ok := []string{"6283131234567", "6283212345678", "6283398765432", "6283876543210"}
	no := []string{"6281234567890", "6285512345678", "62831", "", "6283113"}
	for _, m := range ok {
		if !IsAxis(m) {
			t.Errorf("IsAxis(%q) = false, want true", m)
		}
	}
	for _, m := range no {
		if IsAxis(m) {
			t.Errorf("IsAxis(%q) = true, want false", m)
		}
	}
}

func TestToLocal(t *testing.T) {
	if got := ToLocal("6283131234567"); got != "083131234567" {
		t.Errorf("ToLocal = %q", got)
	}
	if got := ToLocal("083131234567"); got != "083131234567" {
		t.Errorf("ToLocal no-op failed: %q", got)
	}
}
