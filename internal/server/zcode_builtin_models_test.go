package server

import (
	"testing"

	"github.com/jjjane233/workbuddy-3in1/internal/zcode"
)

// TestZCodeMergeBuiltinAddsCallableModels 内置「实测可调」模型要进目录，
// 且不覆盖上游实时条目的同 id 真值。
func TestZCodeMergeBuiltinAddsCallableModels(t *testing.T) {
	base := []map[string]any{
		{"id": "GLM-5.2", "name": "GLM-5.2", "vendor": "ZCode 自带", "context_length": int64(123)},
	}
	out := zcodeMergeBuiltin(base, zcode.BuiltinCallableModels())
	ids := map[string]map[string]any{}
	for _, e := range out {
		if id, _ := e["id"].(string); id != "" {
			ids[id] = e
		}
	}
	// 上游已有的同 id 条目：真值不被覆盖
	if got := ids["GLM-5.2"]["context_length"]; got != int64(123) {
		t.Fatalf("existing context_length overwritten: %v", got)
	}
	// 新增的实测可调模型
	for _, want := range []string{"GLM-5.3", "GLM-5.3-Flash", "glm-4.7", "glm-4.6", "glm-5v-turbo"} {
		if _, ok := ids[want]; !ok {
			t.Fatalf("builtin model %q missing from merged catalog", want)
		}
	}
	// 多模态模型带 supports_images
	if v, _ := ids["glm-4.6v"]["supports_images"].(bool); !v {
		t.Fatalf("glm-4.6v should be vision: %+v", ids["glm-4.6v"])
	}
	// 内置条目标了来源，面板可区分
	if ids["GLM-5.3"]["source"] != "builtin-catalog" {
		t.Fatalf("builtin source tag missing: %+v", ids["GLM-5.3"])
	}
}

// TestZCodeMergeBuiltinIdempotent 重复合并不产生重复 id。
func TestZCodeMergeBuiltinIdempotent(t *testing.T) {
	out := zcodeMergeBuiltin(nil, zcode.BuiltinCallableModels())
	out = zcodeMergeBuiltin(out, zcode.BuiltinCallableModels())
	seen := map[string]int{}
	for _, e := range out {
		if id, _ := e["id"].(string); id != "" {
			seen[id]++
		}
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("id %q appeared %d times", id, n)
		}
	}
}
