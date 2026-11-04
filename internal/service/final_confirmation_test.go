package service

import (
	"strings"
	"testing"
)

func TestFinalButtonLabels(t *testing.T) {
	for _, tc := range []struct {
		text, positive, negative string
		valid                    bool
	}{
		{"Буду ✅\nНе смогу 😢", "Буду ✅", "Не смогу 😢", true},
		{"  Поеду  \r\n  Пропущу  ", "Поеду", "Пропущу", true},
		{strings.Repeat("😀", 30) + "\nНет", strings.Repeat("😀", 30), "Нет", true},
		{"", "", "", false},
		{"Да", "", "", false},
		{"Да\n  \nНет", "", "", false},
		{"Да\nНет\nВозможно", "", "", false},
		{"Да\nДа", "", "", false},
		{"Да\nдА", "", "", false},
		{strings.Repeat("😀", 31) + "\nНет", "", "", false},
	} {
		positive, negative, valid := finalButtonLabels(tc.text)
		if valid != tc.valid || positive != tc.positive || negative != tc.negative {
			t.Fatalf("unexpected labels: %q %q %v", positive, negative, valid)
		}
	}
}
