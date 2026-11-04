package resources

import "fmt"

const FinalConfirmationHeader = "Финальное подтверждение участия"

func FinalConfirmationAnswer(state, positiveLabel, negativeLabel string) string {
	switch state {
	case "invited":
		return "Ожидается ответ"
	case "yes":
		if positiveLabel != "" {
			return positiveLabel
		}
		return "Да"
	case "no":
		if negativeLabel != "" {
			return negativeLabel
		}
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
