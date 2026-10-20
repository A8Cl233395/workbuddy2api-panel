package upstream

import (
	"encoding/json"
	"testing"
)

// TestWriteBodyConversationIDOff 默认关闭：body 一个字节都不多（现状零回归）。
func TestWriteBodyConversationIDOff(t *testing.T) {
	in := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	out, _, _ := prepareBodyOpt([]byte(in), false, nil, nil, nil, "uid123456", "conv-abc", true, false)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("出站 body 解析失败: %v (%s)", err, out)
	}
	if _, has := obj["conversation_id"]; has {
		t.Error("开关关闭时不得写入 conversation_id")
	}
	if _, has := obj["prompt_cache_key"]; !has {
		t.Error("prompt_cache_key 应照常注入（与本开关无关）")
	}
}

// TestWriteBodyConversationIDOn 打开时写入与会话头同源的键（issue #169）。
func TestWriteBodyConversationIDOn(t *testing.T) {
	in := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	out, _, _ := prepareBodyOpt([]byte(in), false, nil, nil, nil, "uid123456", "conv-abc", true, true)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("出站 body 解析失败: %v (%s)", err, out)
	}
	got, _ := obj["conversation_id"].(string)
	want := buildCacheKey("uid123456", "conv-abc")
	if got != want {
		t.Errorf("conversation_id = %q, want %q（与会话头同源）", got, want)
	}
	// 同一会话重复请求必须字节稳定（缓存复用的前提）。
	out2, _, _ := prepareBodyOpt([]byte(in), false, nil, nil, nil, "uid123456", "conv-abc", true, true)
	var obj2 map[string]any
	_ = json.Unmarshal(out2, &obj2)
	if obj2["conversation_id"] != got {
		t.Error("同一会话两次请求的 conversation_id 必须相同")
	}
	// 换会话必须换值（不同会话不能共槽）。
	out3, _, _ := prepareBodyOpt([]byte(in), false, nil, nil, nil, "uid123456", "conv-xyz", true, true)
	var obj3 map[string]any
	_ = json.Unmarshal(out3, &obj3)
	if obj3["conversation_id"] == got {
		t.Error("不同会话的 conversation_id 必须不同")
	}
	// 跨账号隔离：同会话不同账号绝不能共槽（与 buildCacheKey 的 uid 段一致）。
	out4, _, _ := prepareBodyOpt([]byte(in), false, nil, nil, nil, "other999", "conv-abc", true, true)
	var obj4 map[string]any
	_ = json.Unmarshal(out4, &obj4)
	if obj4["conversation_id"] == got {
		t.Error("跨账号必须隔离（uid 段进值）")
	}
}

// TestWriteBodyConversationIDKeepsClientValue 客户端已声明会话时不覆盖，且不因开关改变其它字段。
func TestWriteBodyConversationIDKeepsClientValue(t *testing.T) {
	in := `{"model":"glm-5.2","conversation_id":"client-conv","messages":[{"role":"user","content":"hi"}]}`
	out, _, _ := prepareBodyOpt([]byte(in), false, nil, nil, nil, "uid123456", "conv-abc", true, true)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("出站 body 解析失败: %v", err)
	}
	if got := obj["conversation_id"]; got != "client-conv" {
		t.Errorf("conversation_id = %v, want client-conv（客户端原值优先）", got)
	}

	// camelCase 形态同样认作「已有会话」。
	in2 := `{"model":"glm-5.2","conversationId":"client-conv2","messages":[{"role":"user","content":"hi"}]}`
	out2, _, _ := prepareBodyOpt([]byte(in2), false, nil, nil, nil, "uid123456", "conv-abc", true, true)
	var obj2 map[string]any
	_ = json.Unmarshal(out2, &obj2)
	if got := obj2["conversationId"]; got != "client-conv2" {
		t.Errorf("conversationId = %v, want client-conv2", got)
	}
	if _, has := obj2["conversation_id"]; has {
		t.Error("已有 camelCase 会话标识时不应再补 snake 形态（两种同时存在会歧义）")
	}
}

// TestWriteBodyConversationIDSkipsEmptySession 无会话键时不写（不伪造会话）。
func TestWriteBodyConversationIDSkipsEmptySession(t *testing.T) {
	in := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	out, _, _ := prepareBodyOpt([]byte(in), false, nil, nil, nil, "uid123456", "", false, true)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("出站 body 解析失败: %v", err)
	}
	if _, has := obj["conversation_id"]; has {
		t.Error("会话键为空时不得写入 conversation_id")
	}
}
