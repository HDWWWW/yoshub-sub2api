package service

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestOpenAICodexSystemRoleRawBytes(t *testing.T) {
	// Only the two argument positions may change. Do not decode the expected body.
	const wire = " \t\r\n" + `{
  "previous_response_id" : "resp_\u0031", "instructions":"keep \/ \u0061",
  "n":900719925474099312345, "tools":[{"name":"system","role":"system"}],
  "in\u0070ut" : [
    {"role":"user","content":"{\"role\":\"system\"}"},
    {"t\u0079pe":"m\u0065ssage", "r\u006fle" : %s, "content":[{"type":"input_image","image_url":"data:image/png;base64,AA=="}]},
    {"type":"function_call","id":"fc_1","call_id":"call_1","role":"system","arguments":"{\"n\":900719925474099312345}"},
    {"role":%s,"role":"system","content":{"role":"system"}},
    {"role":"user","role":"system"},
    {"type":"function_call_output","call_id":"call_1","role":"system","output":{"role":"system"}},
    {"type":"reasoning","role":"system","encrypted_content":"gAAA\/+==\u003d"}
  ], "metadata":{"input":[{"role":"system"}]}
}` + "\r\n"
	original := fmt.Sprintf(wire, `"s\u0079stem"`, `"system"`)
	updated := fmt.Sprintf(wire, `"developer"`, `"developer"`)
	for _, tt := range []struct {
		name    string
		account *Account
		want    string
	}{
		{"oauth", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, updated},
		{"setup-token", &Account{Platform: PlatformOpenAI, Type: AccountTypeSetupToken}, updated},
		{"api-key", &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, original},
		{"other-platform", &Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth}, original},
		{"nil-account", nil, original},
	} {
		t.Run(tt.name, func(t *testing.T) { checkCodexSystemRoleBytes(t, tt.account, original, tt.want) })
	}
}

func TestOpenAICodexSystemRoleRawNoMatch(t *testing.T) {
	a := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	for _, body := range []string{
		"", " \r\n", `{}`, `{"input":null}`, `{"input":"system"}`, `{"input":{"role":"system"}}`, `{"input":[]}`,
		`{"input":[null,"system",[{"role":"system"}],{},{"role":3},{"role":null},{"role":"System"},{"role":"developer"}]}`,
		`{"input":[{"type":null,"role":"system"},{"type":"","role":"system"},{"type":"unknown","role":"system"}]}`,
		`{"in\u0070ut":[{"role":"user"}],"input":[{"role":"system"}]}`,
	} {
		checkCodexSystemRoleBytes(t, a, body, body)
	}
}

func TestOpenAICodexSystemRoleRawManyMatches(t *testing.T) {
	// Alternating shorter and longer raw tokens makes an offset in the edited
	// output unusable for locating the next original token.
	const pair = `{"role":"system"},{"role":"\u0073\u0079\u0073\u0074\u0065\u006d"},`
	const wantPair = `{"role":"developer"},{"role":"developer"},`
	wrap := func(pair string) string { return `{"input":[` + strings.Repeat(pair, 40) + `{"role":"user"}]}` }
	checkCodexSystemRoleBytes(t, &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, wrap(pair), wrap(wantPair))
}

func checkCodexSystemRoleBytes(t *testing.T, a *Account, original, want string) {
	t.Helper()
	const tail = "caller-owned spare capacity"
	backing := []byte(original + tail)
	body := backing[:len(original)]
	got, changed, err := normalizeOpenAICodexSystemRolesBody(body, a)
	if err != nil || changed != (want != original) || string(got) != want {
		t.Fatalf("changed=%v, err=%v\n got %s\nwant %s", changed, err, got, want)
	}
	old, oldChanged, oldErr := codexSystemRolesSequential44([]byte(original), a)
	if oldErr != nil || oldChanged != changed || string(old) != want {
		t.Fatalf(".44 mismatch: changed=%v, err=%v\n got %s\nwant %s", oldChanged, oldErr, old, want)
	}
	if string(backing) != original+tail {
		t.Fatal("modified the caller's buffer")
	}
	if !changed && len(got) != 0 && &got[0] != &body[0] {
		t.Fatal("no-op did not return the original buffer")
	}
	again, changed, err := normalizeOpenAICodexSystemRolesBody(got, a)
	if err != nil || changed || string(again) != want || (len(got) != 0 && &again[0] != &got[0]) {
		t.Fatal("normalizing the result again should return it untouched")
	}
}

// Frozen .44 baseline for rerunning the comparison without changing checkouts.
func codexSystemRolesSequential44(body []byte, a *Account) ([]byte, bool, error) {
	if !a.IsOpenAIOAuthLike() {
		return body, false, nil
	}
	input := parseRawJSONView(body).Get("input")
	if !input.IsArray() {
		return body, false, nil
	}
	out, changed := body, false
	var err error
	input.ForEach(func(index, item gjson.Result) bool {
		if !item.IsObject() {
			return true
		}
		if typ := item.Get("type"); typ.Exists() && (typ.Type != gjson.String || typ.Str != "message") {
			return true
		}
		if role := item.Get("role"); role.Type != gjson.String || role.Str != "system" {
			return true
		}
		out, err = sjson.SetBytes(out, "input."+strconv.FormatInt(index.Int(), 10)+".role", "developer")
		if err != nil {
			return false
		}
		changed = true
		return true
	})
	if err != nil {
		return body, false, fmt.Errorf("normalize Codex system message role: %w", err)
	}
	return out, changed, nil
}

var codexSystemRoleBenchSink []byte

// Same workload as the native-role audit: 128 messages, 8160 content bytes
// each, with only the number of evenly spaced system roles varying.
// Local helper only, before (.44, 7036a7f6f) / after, measured using the
// current sub-benchmark on each revision. Go 1.27.4, windows/amd64, Ryzen 9
// 9950X3D, -cpu=1 -benchtime=1s -count=5 -benchmem; medians:
//
//	matches  ns/op before/after      B/op before/after   allocs/op before/after
//	0        734357/586416           0/0                 0/0
//	1        1208899/788306          1058656/1048576     8/1
//	8        5841706/684169          16381056/1048576    64/1
//	32       12308584/1259801        66085270/1049344    257/3
func BenchmarkOpenAICodexSystemRoleRaw(b *testing.B) {
	a := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	const items = 128
	for _, matches := range []int{0, 1, 8, 32} {
		var wire strings.Builder
		wire.WriteString(`{"type":"response.create","model":"gpt-5.1","input":[`)
		content := strings.Repeat("x", 8160)
		for i := 0; i < items; i++ {
			if i != 0 {
				wire.WriteByte(',')
			}
			role := "user"
			if matches > 0 && i%(items/matches) == 0 {
				role = "system"
			}
			fmt.Fprintf(&wire, `{"role":"%s","content":"%s"}`, role, content)
		}
		wire.WriteString(`]}`)
		body := []byte(wire.String())
		for _, impl := range []struct {
			name string
			run  func([]byte, *Account) ([]byte, bool, error)
		}{{"legacy44", codexSystemRolesSequential44}, {"current", normalizeOpenAICodexSystemRolesBody}} {
			b.Run(fmt.Sprintf("roles%d/%s", matches, impl.name), func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(body)))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					got, changed, err := impl.run(body, a)
					if err != nil || changed != (matches > 0) {
						b.Fatal(err, changed)
					}
					codexSystemRoleBenchSink = got
				}
			})
		}
	}
}
