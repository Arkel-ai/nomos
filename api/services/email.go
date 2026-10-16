package services

import u "github.com/Arkel-ai/nomos/api/utils"

var emailLog = u.NewLogger("services.email")

// SendInvite delivers an invitation link to the invited address. The
// starter ships a no-op that logs the link (so the flow works end-to-end
// without an email provider), and the admin UI also shows the link so it
// can be copied/shared manually.
//
// To send real email, wire a provider here (Resend, SES, SMTP, …) behind
// a config flag and keep this signature — every caller is already correct.
// Keep secrets out of logs: we log the recipient + the link
// only, never credentials.
func SendInvite(email, link string) {
	emailLog.Infof("invite email (stub — no provider configured) to=%s link=%s", email, link)
}
