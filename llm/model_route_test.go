package llm

import (
	"encoding/json"
	"testing"
)

func TestGatewayCatalogKeepsNestedModelNamespace(t *testing.T) {
	for model, want := range map[string]string{
		"llmgateway/openai/example": "openai/example",
		"openai/example":            "example",
		"llmgateway/example":        "example",
	} {
		raw, err := new(Client).buildBody(Request{Model: model}, true)
		if err != nil {
			t.Fatal(err)
		}
		var body struct{ Model string }
		if err = json.Unmarshal(raw, &body); err != nil || body.Model != want {
			t.Errorf("model %q: got %q, want %q (%v)", model, body.Model, want, err)
		}
	}
}
