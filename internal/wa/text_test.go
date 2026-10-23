package wa

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestExtractText(t *testing.T) {
	cases := []struct {
		name string
		msg  *waE2E.Message
		kind string
		text string
	}{
		{"nil", nil, "unknown", ""},
		{"conversation", &waE2E.Message{Conversation: proto.String("hello")}, "text", "hello"},
		{"extended", &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("link text")}}, "text", "link text"},
		{"image caption", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("seal photo")}}, "image", "[image] seal photo"},
		{"document", &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("BL.pdf")}}, "document", "[document: BL.pdf]"},
		{"audio", &waE2E.Message{AudioMessage: &waE2E.AudioMessage{}}, "audio", "[voice note]"},
		{"protocol", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{}}, "protocol", ""},
		{"device sent", &waE2E.Message{DeviceSentMessage: &waE2E.DeviceSentMessage{Message: &waE2E.Message{Conversation: proto.String("from phone")}}}, "text", "from phone"},
		{"ephemeral", &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{Conversation: proto.String("ttl")}}}, "text", "ttl"},
		{"key distribution only", &waE2E.Message{SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{}}, "protocol", ""},
		{"unknown kind", &waE2E.Message{Call: &waE2E.Call{}}, "other", "[call]"},
	}
	for _, c := range cases {
		k, s := extractText(c.msg)
		if k != c.kind || s != c.text {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.name, k, s, c.kind, c.text)
		}
	}
	if got := preview("héllo wörld", 5); got != "héllo..." {
		t.Errorf("preview = %q", got)
	}
}
