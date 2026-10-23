package mail

import (
	"strconv"
	"strings"
	"text/template"
	"time"
)

// The four mails. Plain text, at most one link each, and nothing a reader
// could mistake for a second action. name is the bridge's display name
// (BRIDGE_NAME).

var bodies = template.Must(template.New("").Parse(`
{{define "signin"}}Hello,

Use this link to sign in to {{.Name}}. It works once and expires in {{.TTL}}.

{{.Link}}

If you did not ask to sign in, ignore this email. Nobody can sign in without the link.

whatsapp-bridge
{{end}}
{{define "request"}}Hello,

You asked to sign in to {{.Name}}. Your request was sent to the people who approve access.

When it is approved you will get another email with a sign-in link. There is nothing else to do until then.

If you did not ask for access, ignore this email.

whatsapp-bridge
{{end}}
{{define "approve"}}Hello,

{{.Requester}} asks for access to {{.Name}}.

Approving lets this person sign in to the bridge and read the WhatsApp chats shared there. To approve, open this link. It works once and expires in {{.TTL}}.

{{.Link}}

If you do not know this person, ignore this email. The request stays pending and can be removed on the Members page.

whatsapp-bridge
{{end}}
{{define "approved"}}Hello,

You now have access to {{.Name}}. Sign in with this link. It works once and expires in {{.TTL}}.

{{.Link}}

Later, enter your email address on the bridge's sign-in page whenever you need a new link.

whatsapp-bridge
{{end}}`))

type mailData struct {
	Name, Link, TTL, Requester string
}

func render(name string, d mailData) string {
	var b strings.Builder
	if err := bodies.ExecuteTemplate(&b, name, d); err != nil {
		panic(err) // the templates are constants; covered by tests
	}
	return b.String()
}

func displayName(name string) string {
	if name = headerSafe(name); name != "" {
		return name
	}
	return "whatsapp-bridge"
}

// SignIn is the sign-in link a person asked for.
func SignIn(to, name, link string, ttl time.Duration) Message {
	n := displayName(name)
	return Message{Kind: KindSignIn, To: to, Subject: "Sign in to " + n,
		Body: render("signin", mailData{Name: n, Link: link, TTL: Duration(ttl)})}
}

// Request tells a requester their request went to the approvers.
func Request(to, name string) Message {
	n := displayName(name)
	return Message{Kind: KindRequest, To: to, Subject: "Your request for access to " + n,
		Body: render("request", mailData{Name: n})}
}

// Approve asks an approver to let requester in.
func Approve(to, name, requester, link string, ttl time.Duration) Message {
	n := displayName(name)
	r := headerSafe(requester)
	return Message{Kind: KindApprove, To: to, Subject: r + " asks for access to " + n,
		Body: render("approve", mailData{Name: n, Link: link, TTL: Duration(ttl), Requester: r})}
}

// Approved tells a person they now have access (after an approval or an
// invitation), with a sign-in link.
func Approved(to, name, link string, ttl time.Duration) Message {
	n := displayName(name)
	return Message{Kind: KindApproved, To: to, Subject: "You now have access to " + n,
		Body: render("approved", mailData{Name: n, Link: link, TTL: Duration(ttl)})}
}

// Duration says a link lifetime in words: "15 minutes", "1 hour", "7 days".
func Duration(d time.Duration) string {
	unit := func(n int, one string) string {
		if n == 1 {
			return "1 " + one
		}
		return strconv.Itoa(n) + " " + one + "s"
	}
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		return unit(int(d/(24*time.Hour)), "day")
	case d >= time.Hour && d%time.Hour == 0:
		return unit(int(d/time.Hour), "hour")
	default:
		return unit(int((d+time.Minute-1)/time.Minute), "minute")
	}
}
