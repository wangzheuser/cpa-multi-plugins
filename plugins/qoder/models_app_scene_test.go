package main

// models_app_scene_test.go — pins the issue #79 fix.
//
// The reporter's finding (jiesou, 2026-11-07): the gateway delivers QoDuck
// (upstream key "lmodel" — vision-capable, low/high/xhigh reasoning with a
// disabled switch, 1M context, limited-time free) in the "app" scene of
// /algo/api/v2/model/list, not the "chat" scene. callModelsAPI extracted
// only the chat scene, so the model was filtered out of discovery.
//
// The fix merges app-unique entries into the chat listing; chat stays
// authoritative for keys both scenes serve.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// qoduckAppSceneFixture is the shape the issue describes: QoDuck arrives
// only in "app", a chat model is re-listed there with different metadata
// (must not pollute the chat entry), an aggregate placeholder and a
// disabled entry ride along in "app" too.
const qoduckAppSceneFixture = `{
  "chat": [
    {"key":"qfmodel","display_name":"Qoder Flash","enable":true,"is_vl":true,
     "max_input_tokens":131072,"price_factor":0.5},
    {"key":"qthmodel","display_name":"Qoder Think","enable":true,"is_reasoning":true,
     "reasoning_efforts":["low","high"],"max_input_tokens":262144}
  ],
  "app": [
    {"key":"lmodel","display_name":"QoDuck","enable":true,"is_vl":true,
     "max_input_tokens":1000000,
     "thinking_config":{"enabled":{"efforts":{"low":{},"high":{},"xhigh":{}}},"disabled":{}}},
    {"key":"qfmodel","display_name":"Qoder Flash (app variant)","enable":true,
     "max_input_tokens":4096,"price_factor":9.9},
    {"key":"auto","display_name":"Auto","enable":true},
    {"key":"disabled-app-model","display_name":"Disabled","enable":false}
  ],
  "developer": [
    {"key":"devmodel","display_name":"Dev Model","enable":true}
  ]
}`

func modelInfoByID(models []pluginapi.ModelInfo) map[string]pluginapi.ModelInfo {
	out := make(map[string]pluginapi.ModelInfo, len(models))
	for _, m := range models {
		out[m.ID] = m
	}
	return out
}

func TestParseModelsListingMergesAppScene(t *testing.T) {
	models, err := parseModelsListing([]byte(qoduckAppSceneFixture))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	byID := modelInfoByID(models)

	// QoDuck (lmodel) is discovered: 1M context, three reasoning levels in
	// canonical order, thinking can be disabled (ZeroAllowed).
	duck, ok := byID["lmodel"]
	if !ok {
		t.Fatalf("lmodel (QoDuck) must be discovered, got %v", idsOf(models))
	}
	if duck.Name != "QoDuck" {
		t.Fatalf("display name = %q, want QoDuck", duck.Name)
	}
	if duck.ContextLength != 1000000 {
		t.Fatalf("context = %d, want 1000000 (1M)", duck.ContextLength)
	}
	if duck.Thinking == nil {
		t.Fatal("QoDuck declares reasoning levels; Thinking must not be nil")
	}
	if got := strings.Join(duck.Thinking.Levels, ","); got != "low,high,xhigh" {
		t.Fatalf("efforts = %q, want low,high,xhigh (canonical order)", got)
	}
	if !duck.Thinking.ZeroAllowed {
		t.Fatal("thinking_config carries a disabled key; ZeroAllowed must be true")
	}

	// The chat entry for a key both scenes serve is NOT polluted by the app
	// variant (chat metadata wins).
	flash, ok := byID["qfmodel"]
	if !ok {
		t.Fatalf("qfmodel must stay discovered, got %v", idsOf(models))
	}
	if flash.Name != "Qoder Flash" {
		t.Fatalf("chat entry must keep its name, got %q", flash.Name)
	}
	if flash.ContextLength != 131072 {
		t.Fatalf("chat entry must keep its context (app variant said 4096), got %d", flash.ContextLength)
	}

	// Aggregate placeholders never merge; disabled entries stay filtered.
	for _, id := range []string{"auto", "disabled-app-model"} {
		if _, ok := byID[id]; ok {
			t.Fatalf("%q must not be discovered", id)
		}
	}
	// The developer scene is not merged either.
	if _, ok := byID["devmodel"]; ok {
		t.Fatal("developer scene entries must not merge")
	}
	// The chat-only model is untouched.
	if think, ok := byID["qthmodel"]; !ok || think.Thinking == nil || len(think.Thinking.Levels) != 2 {
		t.Fatalf("chat-only model must keep its reasoning_efforts dial: %+v", think.Thinking)
	}
}

func idsOf(models []pluginapi.ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

func TestParseModelsListingWithoutAppScene(t *testing.T) {
	body := `{"chat":[{"key":"qfmodel","display_name":"Qoder Flash","enable":true,"max_input_tokens":131072}]}`
	models, err := parseModelsListing([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(models) != 1 || models[0].ID != "qfmodel" {
		t.Fatalf("chat-only listing must pass through, got %v", idsOf(models))
	}
}

// TestParseModelsListingMalformedAppScene: the app scene is enrichment — a
// malformed variant must not break the authoritative chat listing.
func TestParseModelsListingMalformedAppScene(t *testing.T) {
	body := `{"chat":[{"key":"qfmodel","display_name":"Qoder Flash","enable":true}],"app":"not-an-array"}`
	models, err := parseModelsListing([]byte(body))
	if err != nil {
		t.Fatalf("parse must tolerate a malformed app scene: %v", err)
	}
	if len(models) != 1 || models[0].ID != "qfmodel" {
		t.Fatalf("chat listing must pass through, got %v", idsOf(models))
	}
}

func TestParseModelsListingRequiresChatScene(t *testing.T) {
	if _, err := parseModelsListing([]byte(`{"app":[{"key":"lmodel","enable":true}]}`)); err == nil {
		t.Fatal("no chat scene must be an error (chat stays the authoritative scene)")
	}
	if _, err := parseModelsListing([]byte(`{"chat":[{"key":"off","enable":false}]}`)); err == nil {
		t.Fatal("no enabled models must be an error")
	}
	if _, err := parseModelsListing([]byte(`not json`)); err == nil {
		t.Fatal("invalid json must be an error")
	}
}

// TestMergeAppSceneModelsUnit: the merge in isolation — duplicates by
// trimmed key, empty keys, and slice-aliasing safety.
func TestMergeAppSceneModelsUnit(t *testing.T) {
	var resp map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{"app":[{"key":" lmodel ","enable":true},{"key":""},{"key":"lmodel"}]}`), &resp); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	chat := []chatSceneModel{{Key: "qfmodel", Enable: true}}
	merged := mergeAppSceneModels(resp, chat)
	if len(merged) != 2 {
		t.Fatalf("trimmed-key duplicate and empty key must not merge, got %d", len(merged))
	}
	if merged[0].Key != "qfmodel" || merged[1].Key != " lmodel " {
		t.Fatalf("merge order must be chat-first then app-unique, got %q then %q", merged[0].Key, merged[1].Key)
	}
}
