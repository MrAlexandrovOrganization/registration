package domain

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	for _, tc := range []struct {
		field, input, want string
		bad                bool
	}{
		{"phone", "8 (999) 123-45-67", "79991234567", false}, {"phone", "+12025550123", "12025550123", false},
		{"phone", "abc1234567890", "", true}, {"birth_date", "29.02.2000", "29.02.2000", false}, {"birth_date", "29.02.2001", "", true},
		{"birth_date", "01.01.2999", "", true}, {"group", " рк6-56б ", "РК6-56Б", false},
		{"birth_date", "10.3.2002", "10.03.2002", false},
		{"birth_date", "1.03.2002", "01.03.2002", false},
		{"birth_date", "01.3.2002", "01.03.2002", false},
		{"birth_date", " 1.1.2002 ", "01.01.2002", false},
		{"birth_date", "29.2.2000", "29.02.2000", false},
		{"birth_date", "29.2.2001", "", true},
		{"birth_date", "31.4.2002", "", true},
		{"birth_date", "0.1.2002", "", true},
		{"birth_date", "1.0.2002", "", true},
		{"birth_date", "1.1.02", "", true},
		{"birth_date", "1/1/2002", "", true},
		{"birth_date", "1.1.2002x", "", true},
		{"birth_date", "001.1.2002", "", true},
		{"birth_date", "1.1.1899", "", true},
		{"birth_date", "1.1.1900", "01.01.1900", false},
		{"expectations", strings.Repeat("я", 501), "", true}, {"will_drive", "yes", "", true}, {"trip_attendance", Options["trip_attendance"][1], Options["trip_attendance"][1], false},
	} {
		t.Run(tc.field+tc.input[:min(len(tc.input), 12)], func(t *testing.T) {
			got, err := Normalize(tc.field, tc.input)
			if (err != nil) != tc.bad || (!tc.bad && got != tc.want) {
				t.Fatalf("unexpected normalization: %q %v", got, err)
			}
		})
	}
}
func TestNextDoesNotAcceptMissingOrInvalid(t *testing.T) {
	v := map[string]string{"name": "Fixture User", "birth_date": "29.02.2001"}
	if Next(v) != "birth_date" {
		t.Fatal("invalid nonempty date accepted")
	}
	v["birth_date"] = "29.02.2000"
	if Next(v) != "group" {
		t.Fatal("missing group skipped")
	}
}

func TestRegistrationWithoutParticipation(t *testing.T) {
	values := map[string]string{"name": "Fixture User", "birth_date": "01.01.2000", "group": "test", "phone": "79991234567", "expectations": "test"}
	if NextRegistration(values, false) != "confirm" {
		t.Fatal("disabled questions still required")
	}
	if NextRegistration(values, true) != "will_drive" {
		t.Fatal("enabling participation lost questions")
	}
	if len(Fields) != 7 {
		t.Fatal("SQL column order changed")
	}
}
