package writer

import (
	"context"
	"testing"
)

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

func TestParseEstimate(t *testing.T) {
	e, err := ParseEstimate("Ось:\n" + `{"clarity":{"score":2,"why":"мало"},"questions":["Який трекер?","- Де кнопка?"],"size":"Medium","parts":"web","risk":{"score":3,"why":"налаштування"},"plan":true,"split":true,"model":"gpt","advice":"розбити"}`)
	if err != nil {
		t.Fatal(err)
	}
	if e.State != "done" || e.Size != "medium" || e.Clarity.Value != 2 || e.Risk.Why != "налаштування" || !e.Plan || !e.Split ||
		e.Model != "" || e.Questions != "- Який трекер?\n- Де кнопка?" || e.Advice != "розбити" {
		t.Fatalf("%+v", e)
	}
	for _, bad := range []string{
		`{"clarity":{"score":6},"size":"small","risk":{"score":1}}`,
		`{"clarity":{"score":3},"size":"huge","risk":{"score":1}}`,
		`{"clarity":{"score":3},"size":"small"}`,
		"не JSON",
	} {
		if _, err := ParseEstimate(bad); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}

func TestParseResult(t *testing.T) {
	out := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"LGTM","total_cost_usd":0.12,` +
		`"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":300,"cache_creation_input_tokens":40}}` + "\n")
	text, spend, ok := ParseResult(out)
	if !ok || text != "LGTM" || spend.USD != 0.12 || spend.Tokens != 370 {
		t.Fatalf("%q %+v %v", text, spend, ok)
	}
	if _, _, ok := ParseResult([]byte("## Що зроблено\n- кнопка")); ok {
		t.Fatal("plain text read as a result")
	}
	var got Spend
	ctx := WithSpend(context.Background(), func(s Spend) { got = s })
	if answer(ctx, out) != "LGTM" || got.USD != spend.USD || got.Tokens != spend.Tokens {
		t.Fatalf("spend %+v", got)
	}
	if answer(ctx, []byte("plain")) != "plain" {
		t.Fatal("plain text lost")
	}
}

func TestParseStreamResult(t *testing.T) {
	out := []byte(`{"type":"system","subtype":"init","session_id":"s-9"}
{"type":"assistant","parent_tool_use_id":null,"message":{"id":"m1","usage":{"input_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":15000,"output_tokens":30},"content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"x.go"}}]}}
{"type":"user","parent_tool_use_id":null,"message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"package x"}]}}
{"type":"assistant","parent_tool_use_id":null,"message":{"id":"m2","usage":{"input_tokens":5,"cache_read_input_tokens":15000,"cache_creation_input_tokens":500,"output_tokens":40},"content":[{"type":"text","text":"Добре"}]}}
{"type":"result","subtype":"success","result":"Добре","total_cost_usd":0.05,"usage":{"input_tokens":10,"output_tokens":70,"cache_read_input_tokens":15000,"cache_creation_input_tokens":15500},"modelUsage":{"claude-sonnet-5-5":{"contextWindow":1000000}}}
`)
	text, spend, ok := ParseResult(out)
	if !ok || text != "Добре" || spend.USD != 0.05 || spend.Tokens != 30580 || len(spend.Context) != 1 {
		t.Fatalf("%q %+v %v", text, spend, ok)
	}
	c := spend.Context[0]
	if c.Session != "s-9" || c.Start != 15005 || c.Peak != 15505 || c.Window != 1_000_000 || c.Calls != 2 || c.Tools["Read"] != 470 {
		t.Fatalf("%+v", c)
	}
}
