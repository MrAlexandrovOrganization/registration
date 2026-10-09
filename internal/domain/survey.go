package domain

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Fields is the storage order used by SQL scanning and saving. Do not hide UI
// fields by removing columns here: saved answers must survive feature switches.
var Fields = []string{"name", "birth_date", "group", "phone", "expectations", "will_drive", "trip_attendance"}

func RegistrationFields(participation bool) []string {
	if participation {
		return Fields
	}
	return Fields[:5]
}

var Options = map[string][]string{
	"will_drive":      {"Обязательно! 🤩", "Пока думаю 🤔", "Не смогу 😢"},
	"trip_attendance": {"Да, точно еду! ✅", "Нет, не смогу 😢"},
}
var phone = regexp.MustCompile(`^\+?[0-9 ()-]+$`)
var digits = regexp.MustCompile(`\D`)
var birthDate = regexp.MustCompile(`^[0-9]{1,2}\.[0-9]{1,2}\.[0-9]{4}$`)

// Preserve the legacy group structure with an optional /number suffix.
// Normalize case before checking it.
var group = regexp.MustCompile(`^[А-Я]{1,5}[0-9]{0,2}[СИЦ]?-([1-9]|1[0-6])[1-9][АБМТ]?В?(/[0-9]+)?$`)

func Normalize(field, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || utf8.RuneCountInString(value) > 500 {
		return "", errors.New("invalid_value")
	}
	switch field {
	case "name":
		if utf8.RuneCountInString(value) > 150 {
			return "", errors.New("invalid_value")
		}
	case "birth_date":
		if !birthDate.MatchString(value) {
			return "", errors.New("invalid_date")
		}
		date, err := time.Parse("2.1.2006", value)
		if err != nil || date.After(time.Now()) || date.Year() < 1900 {
			return "", errors.New("invalid_date")
		}
		value = date.Format("02.01.2006")
	case "phone":
		if !phone.MatchString(value) {
			return "", errors.New("invalid_phone")
		}
		value = digits.ReplaceAllString(value, "")
		if len(value) == 11 && value[0] == '8' {
			value = "7" + value[1:]
		}
		if len(value) != 11 || value[0] != '7' {
			return "", errors.New("invalid_phone")
		}
	case "group":
		value = strings.ToUpper(value)
		if !group.MatchString(value) {
			return "", errors.New("invalid_group")
		}
	case "expectations":
	case "will_drive", "trip_attendance":
		if slices.Contains(Options[field], value) {
			return value, nil
		}
		return "", errors.New("invalid_option")
	default:
		return "", errors.New("unknown_field")
	}
	return value, nil
}

// Next also asks for invalid legacy values; a nonempty invalid value is never accepted silently.
func Next(values map[string]string) string {
	return NextRegistration(values, true)
}

func NextRegistration(values map[string]string, participation bool) string {
	for _, field := range RegistrationFields(participation) {
		if _, err := Normalize(field, values[field]); err != nil {
			return field
		}
	}
	return "confirm"
}
