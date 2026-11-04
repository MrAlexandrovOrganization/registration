package resources

import "testing"

func TestFinalConfirmationAnswer(t *testing.T) {
	for _, test := range []struct {
		state, positive, negative, want string
	}{
		{"yes", "Да, еду ✅", "Нет, не смогу 😢", "Да, еду ✅"},
		{"no", "Да, еду ✅", "Нет, не смогу 😢", "Нет, не смогу 😢"},
		{"yes", "=1+1", "Отказ", "=1+1"},
		{"yes", "", "", "Да"},
		{"no", "", "", "Нет"},
		{"invited", "Еду", "Отказ", "Ожидается ответ"},
		{"excluded", "Еду", "Отказ", "Исключён из очереди"},
		{"", "Еду", "Отказ", ""},
	} {
		t.Run(test.state+"/"+test.positive, func(t *testing.T) {
			if got := FinalConfirmationAnswer(test.state, test.positive, test.negative); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}
