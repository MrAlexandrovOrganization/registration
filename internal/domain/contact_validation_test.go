package domain

import (
	"strings"
	"testing"
)

func TestPhoneLegacyFormat(t *testing.T) {
	for _, input := range []string{"+79991234567", "89991234567", "79991234567", "8 (999) 123-45-67", " +7 999 123 45 67 "} {
		got, err := Normalize("phone", input)
		if err != nil || got != "79991234567" {
			t.Errorf("Normalize(%q) = %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"9991234567", "+12025550123", "7999123456", "799912345678", "123456789012345", "abc79991234567", "+7 999 123 45 67 доб. 1", "++79991234567", "+７９９９１２３４５６７", "+()"} {
		_, err := Normalize("phone", input)
		if err == nil || err.Error() != "invalid_phone" {
			t.Errorf("Normalize(%q) error = %v", input, err)
		}
	}
}

func TestGroupLegacyFormat(t *testing.T) {
	for _, input := range []string{
		"М9-11", "ИС9-11", "Э9-11", "МС9-11", "МЦ9-11", "М91-11",
		"М91с-11", "М91с-11а", "М91с-11ав", "ИУ7-41", "ФН12-32", "Э5-12",
		"ИУ-11", "ИУ7-169", "ИУ7-11В", "ИУ7-11МВ", "ИУ7ц-11Т", " рк6-71б ",
	} {
		got, err := Normalize("group", input)
		if err != nil || got != strings.ToUpper(strings.TrimSpace(input)) {
			t.Errorf("Normalize(%q) = %q, %v", input, got, err)
		}
	}
	for _, input := range []string{
		"M9-11", "М999-11", "М9-01", "М9-100", "М9-1", "ММММММ-11",
		"М9-11аа", "М9-11авв", "ИУ7-171", "ИУ7-10", "ИУ7-11Г", "ИУ7-11ВА",
		"ИУ7 41", "ИУ7–41", "ИУ7-4 1", "ИУ7-４１", "ИУ7-41\nИУ7-42", "TEST", "123",
	} {
		_, err := Normalize("group", input)
		if err == nil || err.Error() != "invalid_group" {
			t.Errorf("Normalize(%q) error = %v", input, err)
		}
	}
}

func TestNextReasksInvalidStoredContacts(t *testing.T) {
	values := map[string]string{"name": "Fixture", "birth_date": "01.01.2000", "group": "TEST", "phone": "9991234567", "expectations": "Fixture"}
	if NextRegistration(values, false) != "group" || values["group"] != "TEST" {
		t.Fatal("invalid stored group must be requested without overwriting it")
	}
	values["group"] = "ИУ7-41"
	if NextRegistration(values, false) != "phone" || values["phone"] != "9991234567" {
		t.Fatal("invalid stored phone must be requested without overwriting it")
	}
	values["phone"] = "79991234567"
	if NextRegistration(values, false) != "confirm" {
		t.Fatal("valid answers must allow confirmation")
	}
}
