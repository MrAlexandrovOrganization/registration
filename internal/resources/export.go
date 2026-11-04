package resources

import "fmt"

const FinalConfirmationHeader = "Финальное подтверждение участия"

func FinalConfirmationAnswer(state string) string {
	switch state {
	case "invited":
		return "Ожидается ответ"
	case "yes":
		return "Да"
	case "no":
		return "Нет"
	case "excluded":
		return "Исключён из очереди"
	}
	return ""
}

// SurveyAnswerHeader names a survey column without exposing storage field names.
func SurveyAnswerHeader(title string, id int64) string {
	return fmt.Sprintf("Ответ на вопрос «%s» (№%d)", title, id)
}
