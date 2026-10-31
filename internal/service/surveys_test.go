package service

import (
	"strings"
	"testing"

	pb "registration.local/backend/api"
)

func TestSurveyOptionsAndCallbackBounds(t *testing.T) {
	for _, text := range []string{"one", "same\nsame", "a\n ", strings.Repeat("x", 61) + "\ny", strings.Repeat("a\n", 10) + "b", strings.Repeat("😀", 31) + "\nb"} {
		if _, ok := surveyOptions(text); ok {
			t.Fatalf("accepted invalid options %q", text)
		}
	}
	options, ok := surveyOptions("  <A> & B \n Другой 😀  ")
	if !ok || options[0] != "<A> & B" || options[1] != "Другой 😀" {
		t.Fatal(options, ok)
	}
	for _, data := range []string{"qa_0_1", "qa_1_-2", "qa_1_2_3", "qa_9999999999999999999999_1", "no_1_2"} {
		if _, _, ok := parseSurveyAnswer(data, "qa_"); ok {
			t.Fatal(data)
		}
	}
	q := surveyQuestion{id: 9223372036854775807, status: "open", options: []surveyOption{{9223372036854775807, "yes"}}}
	v := surveyView(q, "survey_question", "c:9223372036854775807:sq_")
	if len(v.Buttons[0].Data) > 64 {
		t.Fatal("callback exceeds Telegram's byte limit")
	}
	qid, oid, ok := parseSurveyAnswer("qa_1_2", "qa_")
	if !ok || qid != 1 || oid != 2 {
		t.Fatal(qid, oid, ok)
	}
}

func TestSurveyQuestionnaireSelection(t *testing.T) {
	s := &Service{}
	u := user{state: "confirm", version: 7, values: map[string]string{
		"name": "Test", "birth_date": "01.01.2000", "group": "ИУ7-41", "phone": "79991234567", "expectations": "Test",
	}, surveys: []surveyQuestion{
		{id: 1, status: "closed", registration: true},
		{id: 2, status: "open", registration: false},
		{id: 3, status: "open", registration: true, answer: 30, options: []surveyOption{{30, "A"}}},
		{id: 4, status: "open", registration: true, options: []surveyOption{{40, "B"}}},
	}}
	if s.nextUser(u) != "survey_4" {
		t.Fatal("must ask only missing active registration questions")
	}
	u.state = "survey_4"
	v := s.view(u)
	if v.Kind != "survey_question" || len(v.Buttons) != 1 || v.Buttons[0].Data != "c:7:sq_4_40" {
		t.Fatal(v)
	}
	u.state = "edit"
	v = s.view(u)
	for _, b := range v.Buttons {
		if strings.Contains(b.Data, "survey_") {
			t.Fatal("survey answer offered for editing")
		}
	}
	u.surveys[3].answer = 40
	if s.nextUser(u) != "confirm" {
		t.Fatal("answered survey asked twice")
	}
	u.values["phone"] = ""
	if s.nextUser(u) != "phone" {
		t.Fatal("base form must precede additional questions")
	}
}

func TestSurveyEditTargets(t *testing.T) {
	update := &pb.Update{Actor: 1, Chat: 1, ChatType: "private", Callback: "qa_1_2", MessageId: 12, CallbackMessageEditable: true}
	for _, kind := range []string{"survey_question"} {
		if editTarget(update, &pb.View{Kind: kind}) != 12 {
			t.Fatal(kind)
		}
	}
	update.CallbackMessageEditable = false
	if editTarget(update, &pb.View{Kind: "survey_answered"}) != 0 {
		t.Fatal("unattested target edited")
	}
}

func TestSurveyAnswersArePrivateInQuestionnaire(t *testing.T) {
	s := &Service{}
	u := user{version: 7, values: map[string]string{"name": "Synthetic"}, surveys: []surveyQuestion{{id: 1, title: "agreement_1", status: "open", registration: true, answer: 2, options: []surveyOption{{2, "YES"}}}}}
	for _, state := range []string{"registered", "confirm", "edit"} {
		u.state = state
		v := s.view(u)
		for _, f := range v.Fields {
			if strings.HasPrefix(f.Key, "survey_") || f.Value == "YES" {
				t.Fatal("private survey answer exposed", v)
			}
		}
		for _, b := range v.Buttons {
			if strings.Contains(b.Data, "survey_") || b.Label == "agreement_1" {
				t.Fatal("survey answer editing exposed", v)
			}
		}
	}
}
