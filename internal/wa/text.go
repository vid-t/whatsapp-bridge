package wa

import (
	"sort"
	"strings"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// extractText classifies a message and returns (kind, text). Media messages
// yield a bracketed description plus any caption. Wrapper messages (phone-sent
// copies, ephemeral, view-once, edits, captioned documents) are unwrapped.
// Protocol, reaction and key-distribution-only messages return kind
// "protocol"/"reaction" with empty text; callers skip them.
func extractText(m *waE2E.Message) (kind, text string) {
	if m == nil {
		return "unknown", ""
	}
	// A message sent from the phone reaches linked devices wrapped like this.
	if ds := m.GetDeviceSentMessage(); ds != nil && ds.GetMessage() != nil {
		return extractText(ds.GetMessage())
	}
	for _, fp := range []*waE2E.FutureProofMessage{
		m.GetEphemeralMessage(),
		m.GetViewOnceMessage(),
		m.GetViewOnceMessageV2(),
		m.GetViewOnceMessageV2Extension(),
		m.GetEditedMessage(),
		m.GetDocumentWithCaptionMessage(),
	} {
		if fp != nil && fp.GetMessage() != nil {
			return extractText(fp.GetMessage())
		}
	}
	switch {
	case m.GetConversation() != "":
		return "text", m.GetConversation()
	case m.GetExtendedTextMessage() != nil:
		return "text", m.GetExtendedTextMessage().GetText()
	case m.GetImageMessage() != nil:
		return "image", withCaption("[image]", m.GetImageMessage().GetCaption())
	case m.GetVideoMessage() != nil:
		return "video", withCaption("[video]", m.GetVideoMessage().GetCaption())
	case m.GetDocumentMessage() != nil:
		d := m.GetDocumentMessage()
		name := d.GetFileName()
		if name == "" {
			name = d.GetMimetype()
		}
		return "document", withCaption("[document: "+name+"]", d.GetCaption())
	case m.GetAudioMessage() != nil:
		return "audio", "[voice note]"
	case m.GetStickerMessage() != nil:
		return "sticker", "[sticker]"
	case m.GetLocationMessage() != nil:
		return "location", "[location]"
	case m.GetContactMessage() != nil:
		return "contact", "[contact]"
	case m.GetPollCreationMessageV3() != nil:
		return "poll", withCaption("[poll]", m.GetPollCreationMessageV3().GetName())
	case m.GetPollCreationMessage() != nil:
		return "poll", withCaption("[poll]", m.GetPollCreationMessage().GetName())
	case m.GetEventMessage() != nil:
		return "event", "[event]"
	case m.GetAlbumMessage() != nil:
		return "album", "[album]"
	case m.GetReactionMessage() != nil:
		return "reaction", ""
	case m.GetProtocolMessage() != nil:
		return "protocol", ""
	}
	fields := setFields(m)
	if len(fields) == 0 {
		// Only key distribution or context info: carries no content.
		return "protocol", ""
	}
	// Unknown content: name the populated fields so it can be diagnosed later.
	return "other", "[" + strings.Join(fields, ",") + "]"
}

// setFields lists populated top-level fields, excluding bookkeeping ones.
func setFields(m *waE2E.Message) []string {
	var names []string
	m.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		switch fd.Name() {
		case "messageContextInfo", "senderKeyDistributionMessage":
			return true
		}
		names = append(names, string(fd.Name()))
		return true
	})
	sort.Strings(names)
	return names
}

func withCaption(label, caption string) string {
	caption = strings.TrimSpace(caption)
	if caption == "" {
		return label
	}
	return label + " " + caption
}

// preview truncates text (by runes) for broadcast events.
func preview(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
