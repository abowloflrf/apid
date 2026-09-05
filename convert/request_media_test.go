package convert

import (
	"encoding/json"
	"testing"
)

func TestToolOutputMediaPreservesNumbers(t *testing.T) {
	quote := func(text string) json.RawMessage {
		b, err := json.Marshal(text)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, number := range []string{"9007199254740993", "-9223372036854775808", "0.1234567890123456789", "1e400"} {
		t.Run(number, func(t *testing.T) {
			payload := `{"id":` + number + `,"content":[{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}`
			for _, tc := range []struct {
				name   string
				raw    json.RawMessage
				nested bool
			}{
				{name: "object", raw: json.RawMessage(payload)},
				{name: "JSON string", raw: quote(payload)},
				{name: "nested JSON string", raw: json.RawMessage(`{"content":[` + string(quote(payload)) + `]}`), nested: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					plan := planToolOutputMedia(tc.raw)
					if plan == nil || len(plan.media) != 1 {
						t.Fatalf("image extraction failed: %+v", plan)
					}
					if image := plan.media[0].ImageURL; image == nil || image.URL != "data:image/png;base64,AA==" {
						t.Fatalf("image changed: %+v", image)
					}
					output := string(plan.output)
					if tc.raw[0] == '"' {
						if err := json.Unmarshal(plan.output, &output); err != nil {
							t.Fatal(err)
						}
					}
					if output != plan.content {
						t.Fatalf("output = %s, want %s", output, plan.content)
					}
					content := plan.content
					if tc.nested {
						var wrapper struct {
							Content []string `json:"content"`
						}
						if err := json.Unmarshal([]byte(content), &wrapper); err != nil {
							t.Fatal(err)
						}
						if len(wrapper.Content) != 1 {
							t.Fatalf("nested content = %+v", wrapper.Content)
						}
						content = wrapper.Content[0]
					}
					var fields map[string]json.RawMessage
					if err := json.Unmarshal([]byte(content), &fields); err != nil {
						t.Fatal(err)
					}
					if got := string(fields["id"]); got != number {
						t.Fatalf("id = %s, want %s", got, number)
					}
				})
			}
		})
	}
}

func TestToolOutputMediaRejectsTrailingJSON(t *testing.T) {
	image := `{"type":"input_image","image_url":"data:image/png;base64,AA=="}`
	for _, suffix := range []string{" {}", " invalid"} {
		raw, err := json.Marshal(image + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if plan := planToolOutputMedia(raw); plan != nil {
			t.Fatalf("text with trailing data was rewritten: %+v", plan)
		}
	}
}
