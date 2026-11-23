package pluginapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestServeRunsOneHookAndAnswersInTheContract(t *testing.T) {
	handlers := map[string]Handler{
		"provision": func(_ context.Context, req Request) (Answer, error) {
			var in AppInput
			if err := req.Decode(&in); err != nil {
				return Answer{}, err
			}
			return Answer{OK: true}.WithOutput(Provisioned{Env: map[string]string{"NOTES_DIR": req.StateDir + "/" + in.App}, Secrets: map[string]string{"key": "k"}})
		},
		"repair": func(context.Context, Request) (Answer, error) {
			return Answer{Messages: []string{"tried"}}, errors.New("the service did not start")
		},
	}
	run := func(hook, request string) (Answer, int) {
		var out bytes.Buffer
		code := Serve(context.Background(), hook, strings.NewReader(request), &out, handlers)
		var answer Answer
		if err := json.Unmarshal(out.Bytes(), &answer); err != nil {
			t.Fatalf("%s: answer is not JSON: %q", hook, out.String())
		}
		return answer, code
	}
	answer, code := run("provision", `{"contract":1,"hook":"provision","state_dir":"/var/lib/x","server":{"arch":"amd64","cpu_flags":["sse4_2"]},"input":{"app":"shop","account":{"user":"app-shop","uid":1001}}}`)
	var out Provisioned
	json.Unmarshal(answer.Output, &out)
	if code != 0 || !answer.OK || out.Env["NOTES_DIR"] != "/var/lib/x/shop" || out.Secrets["key"] != "k" {
		t.Fatalf("provision: %d %+v %+v", code, answer, out)
	}
	// A handler's error is an answer that is not ok, with its messages kept.
	if answer, code = run("repair", `{"contract":1}`); code != 1 || answer.OK || strings.Join(answer.Messages, "|") != "tried|the service did not start" {
		t.Fatalf("failing hook: %d %+v", code, answer)
	}
	if answer, code = run("backup", `{"contract":1}`); code != 1 || answer.OK || !strings.Contains(answer.Messages[0], `does not implement the "backup" hook`) {
		t.Fatalf("unknown hook: %d %+v", code, answer)
	}
	if answer, code = run("provision", `{"contract":2}`); code != 1 || !strings.Contains(answer.Messages[0], "speaks contract 1, the agent sent 2") {
		t.Fatalf("other contract: %d %+v", code, answer)
	}
	if answer, code = run("provision", `not json`); code != 1 || answer.OK {
		t.Fatalf("invalid request: %d %+v", code, answer)
	}
	if !(Server{CPUFlags: []string{"avx", "avx2"}}).HasCPUFlag("avx2") || (Server{}).HasCPUFlag("avx2") {
		t.Fatal("processor flags")
	}
}
