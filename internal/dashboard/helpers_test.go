package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMaskTokenEscapedForms 覆盖含 JSON 转义字符的 token：meta.json 里
// token 以转义形态（\"、\\）出现，只遮原始字节会静默漏遮；以 \ 结尾的
// token 还要求先替换转义形态，否则残留孤立反斜杠破坏 JSON。
func TestMaskTokenEscapedForms(t *testing.T) {
	handler, err := New("pw", "https://example.com", nil, "", false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	token := `sk-a"b\c` // 含 " 且以 \ 结尾
	handler.tokenFunc = func() string { return token }

	meta, err := json.Marshal(map[string]string{"token_field": token})
	if err != nil {
		t.Fatal(err)
	}
	masked := handler.maskToken(meta)
	if strings.Contains(string(masked), "sk-a") {
		t.Fatalf("escaped token residue in masked output: %s", masked)
	}
	var decoded map[string]string
	if err := json.Unmarshal(masked, &decoded); err != nil {
		t.Fatalf("masked meta is not valid JSON: %v (%s)", err, masked)
	}

	// 原始形态（非 JSON 上下文）同样要遮。
	raw := handler.maskToken([]byte("token=" + token))
	if strings.Contains(string(raw), "sk-a") {
		t.Fatalf("raw token residue: %s", raw)
	}
}
