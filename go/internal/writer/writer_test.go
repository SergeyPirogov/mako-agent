package writer

import "testing"

func TestReviewNotes(t *testing.T) {
	for in, want := range map[string]string{
		"LGTM":                     "",
		"  lgtm.\n":                "",
		"**LGTM**":                 "",
		"```\n- a.go:3: тест\n```": "- a.go:3: тест",
		"- a.go:3: немає перевірки": "- a.go:3: немає перевірки",
	} {
		got, err := ReviewNotes(in)
		if err != nil || got != want {
			t.Errorf("ReviewNotes(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ReviewNotes("  "); err == nil {
		t.Error("an empty answer passed")
	}
}

func TestParseAssessment(t *testing.T) {
	a, err := ParseAssessment("Ось оцінка:\n```json\n" + `{"clarity":{"score":2,"why":" Не сказано де "},"complexity":{"score":3,"why":"x"},"completeness":{"score":5,"why":"y"},"risk":{"score":1,"why":"z"},"size":" Big","missing":["де кнопка","- який текст"]}` + "\n```")
	if err != nil {
		t.Fatal(err)
	}
	if a.State != "done" || a.Clarity.Value != 2 || a.Clarity.Why != "Не сказано де" || a.Complexity.Value != 3 || a.Completeness.Value != 5 || a.Risk.Value != 1 || a.Size != "big" {
		t.Fatalf("%+v", a)
	}
	if a.Missing != "- де кнопка\n- який текст" {
		t.Fatalf("missing %q", a.Missing)
	}
	a, err = ParseAssessment(`{"clarity":{"score":5},"complexity":{"score":1},"completeness":{"score":4},"risk":{"score":2},"size":"huge","missing":""}`)
	if err != nil || a.Missing != "" || a.Clarity.Value != 5 || a.Size != "" {
		t.Fatalf("%+v, %v", a, err)
	}
	for _, bad := range []string{"немає", `{"clarity":{"score":0},"complexity":{"score":1},"completeness":{"score":1},"risk":{"score":1}}`, `{"clarity":{"score":6}}`} {
		if _, err := ParseAssessment(bad); err == nil {
			t.Errorf("ParseAssessment(%q) passed", bad)
		}
	}
}

func TestParseRetro(t *testing.T) {
	r, err := ParseRetro("```json\n" + `{"good":["Зроблено з першої спроби"],"bad":"- Двічі правили тест","next":[]}` + "\n```")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != "done" || r.Good != "- Зроблено з першої спроби" || r.Bad != "- Двічі правили тест" || r.Next != "" {
		t.Fatalf("%+v", r)
	}
	for _, bad := range []string{"немає", `{"good":[],"bad":[],"next":""}`} {
		if _, err := ParseRetro(bad); err == nil {
			t.Errorf("ParseRetro(%q) passed", bad)
		}
	}
}
