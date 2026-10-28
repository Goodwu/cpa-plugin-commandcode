package main

import "testing"

func TestParseCCLineFlat(t *testing.T) {
	event, ok := parseCCLine(`{"type":"text-delta","id":"txt-0","text":"4"}`)
	if !ok {
		t.Fatal("expected event")
	}
	if event.Type != "text-delta" {
		t.Fatalf("type = %q", event.Type)
	}
	if event.Data["text"] != "4" {
		t.Fatalf("data = %v", event.Data)
	}
	if _, exists := event.Data["id"]; exists {
		t.Fatal("id must be stripped from flat data")
	}
	if _, exists := event.Data["type"]; exists {
		t.Fatal("type must be stripped from flat data")
	}
}

func TestParseCCLineNestedAndPrefix(t *testing.T) {
	event, ok := parseCCLine("data: {\"type\":\"text-delta\",\"data\":{\"text\":\"Hello\"}}")
	if !ok {
		t.Fatal("expected event")
	}
	if event.Data["text"] != "Hello" {
		t.Fatalf("nested data not used: %v", event.Data)
	}
}

func TestParseCCLineDoneAndNoise(t *testing.T) {
	for _, line := range []string{"", "   ", "data: [DONE]", "keepalive", "{not json", `{"no_type":1}`} {
		if _, ok := parseCCLine(line); ok {
			t.Fatalf("line %q should not parse to an event", line)
		}
	}
}

func TestParseCCLineEmptyNestedObject(t *testing.T) {
	event, ok := parseCCLine(`{"type":"finish","data":{}}`)
	if !ok {
		t.Fatal("expected event")
	}
	if _, exists := event.Data["data"]; exists {
		t.Fatalf("nested object must be used as data, got %v", event.Data)
	}
}
