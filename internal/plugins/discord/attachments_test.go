package discord

import (
	"encoding/json"
	"testing"
)

// MESSAGE_CREATE attachments decode into Inbound.Attachments with the CDN
// metadata intact; messages without attachments keep a nil slice.
func TestDispatchMessage_Attachments(t *testing.T) {
	var got []Inbound
	g := &Gateway{Token: "tok", OnMessage: func(in Inbound) { got = append(got, in) }, seen: newSeenSet(8)}

	g.dispatchMessage(json.RawMessage(`{
		"id": "m1", "channel_id": "c1", "content": "here's the file",
		"author": {"id": "u1", "bot": false},
		"attachments": [
			{"id": "a1", "filename": "report.pdf", "size": 12345,
			 "content_type": "application/pdf",
			 "url": "https://cdn.discordapp.com/attachments/1/2/report.pdf?ex=x&is=y&hm=z"},
			{"id": "a2", "filename": "pic.png", "size": 99,
			 "content_type": "image/png",
			 "url": "https://cdn.discordapp.com/attachments/1/3/pic.png"}
		]
	}`))
	g.dispatchMessage(json.RawMessage(`{"id":"m2","channel_id":"c1","content":"plain","author":{"id":"u1"}}`))

	if len(got) != 2 {
		t.Fatalf("want 2 inbounds, got %d", len(got))
	}
	atts := got[0].Attachments
	if len(atts) != 2 {
		t.Fatalf("want 2 attachments, got %+v", atts)
	}
	if atts[0].Filename != "report.pdf" || atts[0].Size != 12345 ||
		atts[0].ContentType != "application/pdf" || atts[0].URL == "" {
		t.Fatalf("attachment 0 mis-decoded: %+v", atts[0])
	}
	if got[1].Attachments != nil {
		t.Fatalf("no-attachment message must carry nil slice, got %+v", got[1].Attachments)
	}
}
