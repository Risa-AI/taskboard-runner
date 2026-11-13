package protocol

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Risa-AI/taskboard-runner/pkg/apitest"
)

func str(s string) *string { return &s }

// A runner already released keeps talking to every later taskboard, so a
// field's JSON name is a promise: renaming one breaks every runner in the
// field without a compile error on either side. Each value below must
// encode to exactly its JSON, and that JSON must decode back to the value.
func TestJSON_FieldNamesArePinned(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    interface{}
		json string
	}{
		{"environments report", EnvironmentsReport{{Name: "staging", Secrets: []string{"admin_password", "api_key"}}},
			`[{"name":"staging","secrets":["admin_password","api_key"]}]`},
		{"heartbeat", Heartbeat{Version: "v0.3.0", SlotsFree: 1},
			`{"version":"v0.3.0","slots_free":1}`},
		{"heartbeat answer", HeartbeatAnswer{RunsToCancel: []string{"run-7"}},
			`{"runs_to_cancel":["run-7"]}`},
		{"job", Job{
			RunID: "run-7",
			Suite: Suite{
				Files:    []File{{Name: "01_auth.yml", Content: "- name: login\n"}},
				Seeds:    apitest.Seeds{"n": {Value: str("0")}},
				Fixtures: []Fixture{{Name: "fixtures/proof.png", URL: "https://files.example.test/f/1"}},
			},
			Environment: JobEnvironment{Name: "staging", BaseURL: "https://staging.example.test", Variables: map[string]string{"admin_email": "qa@example.test"}},
			TimeoutS:    1200,
			LeaseS:      120,
		}, `{"run_id":"run-7","suite":{"files":[{"name":"01_auth.yml","content":"- name: login\n"}],"seeds":{"n":{"value":"0"}},"fixtures":[{"name":"fixtures/proof.png","url":"https://files.example.test/f/1"}]},"environment":{"name":"staging","base_url":"https://staging.example.test","variables":{"admin_email":"qa@example.test"}},"timeout_s":1200,"lease_s":120}`},
		{"case result", CaseResult{Index: 3, File: "02_me.yml", Line: 5, Name: "me", Method: "GET", Path: "/v1/me", Status: 500, Outcome: "fail", DurationMS: 42, Reason: "expected status 200, got 500", Request: "GET /v1/me", Response: `{"held":"[redacted]"}`},
			`{"index":3,"file":"02_me.yml","line":5,"name":"me","method":"GET","path":"/v1/me","status":500,"outcome":"fail","duration_ms":42,"reason":"expected status 200, got 500","request":"GET /v1/me","response":"{\"held\":\"[redacted]\"}"}`},
		{"complete", Complete{Summary: Summary{Verdict: "failed", Total: 4, OK: 2, Skipped: 1, Failed: 1, NotRun: 0}, Error: "secret admin_password of environment staging is empty"},
			`{"summary":{"verdict":"failed","total":4,"ok":2,"skipped":1,"failed":1,"not_run":0},"error":"secret admin_password of environment staging is empty"}`},
		{"complete without error", Complete{Summary: Summary{Verdict: "passed", Total: 1, OK: 1}},
			`{"summary":{"verdict":"passed","total":1,"ok":1,"skipped":0,"failed":0,"not_run":0}}`},
		{"refusal", Envelope[*Job]{Error: &Error{Code: CodeProtocolTooOld, Message: "update the runner to v0.3.0 or newer"}},
			`{"success":false,"error":{"code":"PROTOCOL_TOO_OLD","message":"update the runner to v0.3.0 or newer"}}`},
		{"answer", Envelope[HeartbeatAnswer]{Success: true, Data: HeartbeatAnswer{RunsToCancel: []string{}}},
			`{"success":true,"data":{"runs_to_cancel":[]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.v)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.json {
				t.Errorf("encoded\n got: %s\nwant: %s", got, tc.json)
			}
			back := reflect.New(reflect.TypeOf(tc.v))
			if err := json.Unmarshal([]byte(tc.json), back.Interface()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(back.Elem().Interface(), tc.v) {
				t.Errorf("decoded\n got: %#v\nwant: %#v", back.Elem().Interface(), tc.v)
			}
		})
	}
}

// The version and the header names are read by a taskboard the runner was
// not built with; changing one is a new protocol, never an edit.
func TestProtocol_VersionAndHeadersArePinned(t *testing.T) {
	if ProtocolVersion != 1 {
		t.Errorf("ProtocolVersion = %d, want 1", ProtocolVersion)
	}
	for got, want := range map[string]string{
		BasePath:           "/v1/runner",
		TokenHeader:        "X-Fleet-Token",
		VersionHeader:      "X-Runner-Protocol",
		CodeProtocolTooOld: "PROTOCOL_TOO_OLD",
	} {
		if got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
}
