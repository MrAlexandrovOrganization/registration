package resources

import "fmt"

// SurveyAnswerHeader names a survey column without exposing storage field names.
func SurveyAnswerHeader(title string, id int64) string {
	return fmt.Sprintf("Ответ на вопрос «%s» (№%d)", title, id)
}
